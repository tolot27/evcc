package cmd

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/evcc-io/evcc/core/session"
	"github.com/evcc-io/evcc/db"
	"github.com/evcc-io/evcc/util/config"
	"github.com/evcc-io/evcc/util/templates"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type easeeCall struct {
	serial, url string
	enable      bool
}

func setupEaseeMigrate(t *testing.T, chargerTemplate string) (lp, charger config.Config, calls *[]easeeCall) {
	t.Helper()

	require.NoError(t, db.NewInstance("sqlite", ":memory:"))

	calls = new([]easeeCall)
	oldSet, oldBackup := easeeSetLocalOcpp, backupDatabase
	easeeSetLocalOcpp = func(user, password, serial, url string, enable bool) error {
		*calls = append(*calls, easeeCall{serial, url, enable})
		return nil
	}
	backupDatabase = func(io.Writer) error { return nil }
	t.Cleanup(func() { easeeSetLocalOcpp, backupDatabase = oldSet, oldBackup })

	charger, err := config.AddConfig(templates.Charger, map[string]any{
		"template": chargerTemplate,
		"user":     "me@example.org",
		"password": "secret",
		"charger":  "EH123456",
	}, config.WithProperties(config.Properties{Type: "template", Title: "Garage", Product: "Easee Home"}))
	require.NoError(t, err)

	lp, err = config.AddConfig(templates.Loadpoint, map[string]any{
		"title":   "Garage",
		"charger": config.NameForID(charger.ID),
		"circuit": "db:5",
		"vehicle": "db:6",
	})
	require.NoError(t, err)

	return lp, charger, calls
}

func loadpointData(t *testing.T, lp config.Config) map[string]any {
	t.Helper()
	c, err := config.ConfigByID(lp.ID)
	require.NoError(t, err)
	return c.Data
}

func TestEaseeMigrateRoundtrip(t *testing.T) {
	lp, easeeCharger, calls := setupEaseeMigrate(t, easeeTemplate)

	// dry run changes nothing
	require.NoError(t, easeeMigrateToOcpp(io.Discard, "Garage", easeeOcppTemplate, "ws://1.2.3.4:8887/", "", false))
	assert.Equal(t, config.NameForID(easeeCharger.ID), loadpointData(t, lp)["charger"])
	assert.Empty(t, *calls)

	// to ocpp
	require.NoError(t, easeeMigrateToOcpp(io.Discard, "Garage", easeeOcppTemplate, "ws://1.2.3.4:8887/", "", true))
	data := loadpointData(t, lp)
	assert.Equal(t, "db:6", data["vehicle"])
	assert.Equal(t, "db:5", data["circuit"])
	assert.Equal(t, "Garage", data["title"])
	assert.NotEqual(t, config.NameForID(easeeCharger.ID), data["charger"])
	assert.Equal(t, []easeeCall{{"EH123456", "ws://1.2.3.4:8887/", true}}, *calls)

	id, err := config.IDForName(data["charger"].(string))
	require.NoError(t, err)
	ocpp, err := config.ConfigByID(id)
	require.NoError(t, err)
	assert.Equal(t, easeeOcppTemplate, ocpp.Data["template"])
	assert.Equal(t, "EH123456", ocpp.Data["stationid"])
	assert.Equal(t, "Easee Home (OCPP)", ocpp.Product)

	// back to easee
	require.NoError(t, easeeMigrateToEasee(io.Discard, "Garage", true))
	assert.Equal(t, config.NameForID(easeeCharger.ID), loadpointData(t, lp)["charger"])
	assert.Equal(t, easeeCall{"EH123456", "", false}, (*calls)[1])

	// second round reuses the ocpp charger
	require.NoError(t, easeeMigrateToOcpp(io.Discard, "Garage", easeeOcppTemplate, "ws://1.2.3.4:8887/", "", true))
	assert.Equal(t, config.NameForID(ocpp.ID), loadpointData(t, lp)["charger"])
}

func TestEaseeMigrateGenericTemplate(t *testing.T) {
	lp, _, _ := setupEaseeMigrate(t, easeeTemplate)

	require.NoError(t, easeeMigrateToOcpp(io.Discard, "Garage", ocppTemplate, "ws://1.2.3.4:8887/", "", true))

	id, err := config.IDForName(loadpointData(t, lp)["charger"].(string))
	require.NoError(t, err)
	c, err := config.ConfigByID(id)
	require.NoError(t, err)
	assert.Equal(t, ocppTemplate, c.Data["template"])
	assert.Empty(t, c.Product)
}

func TestEaseeMigrateDisabled(t *testing.T) {
	lp, _, _ := setupEaseeMigrate(t, easeeTemplate)

	lp.Disable = true
	require.NoError(t, lp.Update(lp.Data))

	var buf bytes.Buffer
	require.NoError(t, easeeMigrateToOcpp(&buf, "Garage", easeeOcppTemplate, "ws://1.2.3.4:8887/", "", true))
	assert.Contains(t, buf.String(), "loadpoint is disabled")

	// disabled flag survives the migration
	c, err := config.ConfigByID(lp.ID)
	require.NoError(t, err)
	assert.True(t, c.Disable)
	assert.NotEqual(t, lp.Data["charger"], c.Data["charger"])
}

func TestEaseeMigrateErrors(t *testing.T) {
	t.Run("unknown loadpoint", func(t *testing.T) {
		setupEaseeMigrate(t, easeeTemplate)
		assert.Error(t, easeeMigrateToOcpp(io.Discard, "Nope", easeeOcppTemplate, "ws://x/", "", true))
	})

	t.Run("invalid template", func(t *testing.T) {
		setupEaseeMigrate(t, easeeTemplate)
		assert.Error(t, easeeMigrateToOcpp(io.Discard, "Garage", "foo", "ws://x/", "", true))
	})

	t.Run("not an easee charger", func(t *testing.T) {
		setupEaseeMigrate(t, "demo-charger")
		assert.Error(t, easeeMigrateToOcpp(io.Discard, "Garage", easeeOcppTemplate, "ws://x/", "", true))
		assert.Error(t, easeeMigrateToEasee(io.Discard, "Garage", true))
	})

	t.Run("open session", func(t *testing.T) {
		lp, charger, calls := setupEaseeMigrate(t, easeeTemplate)
		require.NoError(t, db.Instance.Create(&session.Session{Loadpoint: "Garage", Created: time.Now()}).Error)

		assert.ErrorContains(t, easeeMigrateToOcpp(io.Discard, "Garage", easeeOcppTemplate, "ws://x/", "", true), "open charging session")
		assert.Equal(t, config.NameForID(charger.ID), loadpointData(t, lp)["charger"])
		assert.Empty(t, *calls)
	})

	t.Run("missing serial", func(t *testing.T) {
		_, charger, _ := setupEaseeMigrate(t, easeeTemplate)
		data := charger.Data
		delete(data, "charger")
		require.NoError(t, charger.Update(data))

		assert.ErrorContains(t, easeeMigrateToOcpp(io.Discard, "Garage", easeeOcppTemplate, "ws://x/", "", true), "--serial")
		require.NoError(t, easeeMigrateToOcpp(io.Discard, "Garage", easeeOcppTemplate, "ws://x/", "EH999", true))
	})

	t.Run("no easee charger for rollback", func(t *testing.T) {
		lp, charger, _ := setupEaseeMigrate(t, easeeTemplate)
		require.NoError(t, easeeMigrateToOcpp(io.Discard, "Garage", easeeOcppTemplate, "ws://x/", "", true))
		require.NoError(t, charger.Delete())

		var buf bytes.Buffer
		assert.ErrorContains(t, easeeMigrateToEasee(&buf, "Garage", true), "no Easee cloud charger")
		assert.NotEqual(t, config.NameForID(charger.ID), loadpointData(t, lp)["charger"])
	})
}
