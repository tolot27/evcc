package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/evcc-io/evcc/charger/easee"
	"github.com/evcc-io/evcc/core/session"
	"github.com/evcc-io/evcc/db"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/config"
	"github.com/evcc-io/evcc/util/templates"
	"github.com/spf13/cobra"
)

const (
	easeeTemplate     = "easee"
	easeeOcppTemplate = "ocpp-easee"
	ocppTemplate      = "ocpp"
	ocppPort          = 8887
)

var easeeOcppTemplates = []string{easeeOcppTemplate, ocppTemplate}

var easeeMigrateCmd = &cobra.Command{
	Use:   "easee-migrate",
	Short: "Switch a loadpoint between Easee cloud and local OCPP charger",
	Long: `Switch the charger of a loadpoint between the Easee cloud integration and local OCPP.
Loadpoint, circuit, vehicle, sessions and statistics are kept.

Stop evcc before applying. Without --apply only the planned changes are shown.`,
}

var easeeMigrateToOcppCmd = &cobra.Command{
	Use:   "to-ocpp",
	Short: "Switch loadpoint from Easee cloud to local OCPP",
	Args:  cobra.ExactArgs(0),
	Run:   runEaseeMigrateToOcpp,
}

var easeeMigrateToEaseeCmd = &cobra.Command{
	Use:   "to-easee",
	Short: "Switch loadpoint from local OCPP back to Easee cloud",
	Args:  cobra.ExactArgs(0),
	Run:   runEaseeMigrateToEasee,
}

func init() {
	rootCmd.AddCommand(easeeMigrateCmd)
	easeeMigrateCmd.AddCommand(easeeMigrateToOcppCmd)
	easeeMigrateCmd.AddCommand(easeeMigrateToEaseeCmd)

	easeeMigrateCmd.PersistentFlags().String("loadpoint", "", "Loadpoint title")
	easeeMigrateCmd.PersistentFlags().Bool("apply", false, "Apply changes (default: dry run)")
	_ = easeeMigrateCmd.MarkPersistentFlagRequired("loadpoint")

	easeeMigrateToOcppCmd.Flags().String("url", "", "OCPP Central System URL (default: ws://<evcc ip>:8887/)")
	easeeMigrateToOcppCmd.Flags().String("template", easeeOcppTemplate, "OCPP charger template: "+strings.Join(easeeOcppTemplates, ", "))
	easeeMigrateToOcppCmd.Flags().String("serial", "", "Easee serial number, if not configured at the Easee charger")
}

// easeeSetLocalOcpp switches local OCPP on the Easee charger. Replaced in tests.
var easeeSetLocalOcpp = func(user, password, serial, url string, enable bool) error {
	client, err := easeeOcppClient(user, password)
	if err != nil {
		return err
	}

	if enable {
		return easeeOcppApplyConfig(client, serial, localOcppConnectionDetails{
			ConnectivityMode:        "DualProtocol",
			WebsocketConnectionArgs: &localOcppWebsocketArgs{URL: url},
		})
	}

	var current localOcppConnectionDetails
	if err := client.GetJSON(fmt.Sprintf("%s/v1/connection-details/%s", easee.LocalOcppAPI, serial), &current); err != nil {
		return err
	}

	current.ConnectivityMode = "OcppOff"
	if current.WebsocketConnectionArgs != nil {
		current.WebsocketConnectionArgs.URL = strings.TrimSuffix(current.WebsocketConnectionArgs.URL, serial)
	}

	return easeeOcppApplyConfig(client, serial, current)
}

func runEaseeMigrateToOcpp(cmd *cobra.Command, args []string) {
	title, _ := cmd.Flags().GetString("loadpoint")
	apply, _ := cmd.Flags().GetBool("apply")
	url, _ := cmd.Flags().GetString("url")
	tpl, _ := cmd.Flags().GetString("template")
	serial, _ := cmd.Flags().GetString("serial")

	easeeMigrateSetup(cmd)

	if err := easeeMigrateToOcpp(cmd.OutOrStdout(), title, tpl, url, serial, apply); err != nil {
		log.FATAL.Fatal(err)
	}
}

func runEaseeMigrateToEasee(cmd *cobra.Command, args []string) {
	title, _ := cmd.Flags().GetString("loadpoint")
	apply, _ := cmd.Flags().GetBool("apply")

	easeeMigrateSetup(cmd)

	if err := easeeMigrateToEasee(cmd.OutOrStdout(), title, apply); err != nil {
		log.FATAL.Fatal(err)
	}
}

func easeeMigrateSetup(cmd *cobra.Command) {
	if err := loadConfigFile(&conf, !cmd.Flag(flagIgnoreDatabase).Changed); err != nil {
		log.FATAL.Fatal(err)
	}

	if err := configureDatabase(conf.Database); err != nil {
		log.FATAL.Fatal(err)
	}
}

func easeeMigrateToOcpp(out io.Writer, title, tpl, url, serial string, apply bool) error {
	if !slices.Contains(easeeOcppTemplates, tpl) {
		return fmt.Errorf("invalid template %q, use one of: %s", tpl, strings.Join(easeeOcppTemplates, ", "))
	}

	lp, err := migrateLoadpoint(title)
	if err != nil {
		return err
	}

	old, err := loadpointCharger(lp)
	if err != nil {
		return err
	}

	if old.Data["template"] != easeeTemplate {
		return fmt.Errorf("charger %s of loadpoint %q is not an Easee cloud charger (template: %v)", config.NameForID(old.ID), title, old.Data["template"])
	}

	if serial == "" {
		serial = dataString(old.Data, "charger")
	}
	if serial == "" {
		return errors.New("easee serial number not configured, use --serial")
	}

	if url == "" {
		ips := util.LocalIPs()
		if len(ips) == 0 {
			return errors.New("cannot determine evcc ip address, use --url")
		}
		url = fmt.Sprintf("ws://%s:%d/", ips[0].IP, ocppPort)
		if len(ips) > 1 {
			fmt.Fprintf(out, "warning: multiple ip addresses found, using %s (override with --url)\n", ips[0].IP)
		}
	}

	if err := ensureNoOpenSession(title); err != nil {
		return err
	}

	existing, err := findOcppCharger(tpl, serial)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "loadpoint:       %s (%s)\n", title, config.NameForID(lp.ID))
	fmt.Fprintf(out, "old charger:     %s (template %s, serial %s)\n", config.NameForID(old.ID), easeeTemplate, serial)
	if existing != nil {
		fmt.Fprintf(out, "new charger:     %s (existing, template %s)\n", config.NameForID(existing.ID), tpl)
	} else {
		fmt.Fprintf(out, "new charger:     create (template %s, stationid %s)\n", tpl, serial)
	}
	fmt.Fprintf(out, "easee ocpp url:  %s\n", url)
	fmt.Fprintln(out, "note: requires Easee firmware >= 328 and evcc sponsorship; stop evcc before applying")

	if !apply {
		fmt.Fprintln(out, "dry run, use --apply to execute")
		return nil
	}

	if err := backupDatabase(out); err != nil {
		return err
	}

	user, password := dataString(old.Data, "user"), dataString(old.Data, "password")
	if err := easeeSetLocalOcpp(user, password, serial, url, true); err != nil {
		return fmt.Errorf("enabling local ocpp at easee: %w", err)
	}

	created := false
	if existing == nil {
		props := old.Properties
		props.Product = ""
		if tpl == easeeOcppTemplate && old.Product != "" {
			props.Product = old.Product + " (OCPP)"
		}

		c, err := config.AddConfig(templates.Charger, map[string]any{
			"template":  tpl,
			"stationid": serial,
			"connector": 1,
		}, config.WithProperties(config.Properties{
			Type:    "template",
			Title:   props.Title,
			Icon:    props.Icon,
			Product: props.Product,
		}))
		if err != nil {
			return err
		}
		existing, created = &c, true
	}

	if err := setLoadpointCharger(lp, config.NameForID(existing.ID)); err != nil {
		if created {
			_ = existing.Delete()
		}
		return err
	}

	fmt.Fprintf(out, "loadpoint %q now uses %s. Charger connects to evcc on port %d once evcc is started.\n", title, config.NameForID(existing.ID), ocppPort)
	return nil
}

func easeeMigrateToEasee(out io.Writer, title string, apply bool) error {
	lp, err := migrateLoadpoint(title)
	if err != nil {
		return err
	}

	cur, err := loadpointCharger(lp)
	if err != nil {
		return err
	}

	if tpl, _ := cur.Data["template"].(string); !slices.Contains(easeeOcppTemplates, tpl) {
		return fmt.Errorf("charger %s of loadpoint %q is not an Easee ocpp charger (template: %v)", config.NameForID(cur.ID), title, cur.Data["template"])
	}

	serial := dataString(cur.Data, "stationid")
	if serial == "" {
		return fmt.Errorf("charger %s has no stationid", config.NameForID(cur.ID))
	}

	if err := ensureNoOpenSession(title); err != nil {
		return err
	}

	chargers, err := config.ConfigurationsByClass(templates.Charger)
	if err != nil {
		return err
	}

	var easeeCharger *config.Config
	for _, c := range chargers {
		if c.Data["template"] == easeeTemplate && dataString(c.Data, "charger") == serial {
			easeeCharger = &c
			break
		}
	}
	if easeeCharger == nil {
		return fmt.Errorf("no Easee cloud charger with serial %s found, create it in the UI first", serial)
	}

	fmt.Fprintf(out, "loadpoint:       %s (%s)\n", title, config.NameForID(lp.ID))
	fmt.Fprintf(out, "old charger:     %s (template %v, stationid %s)\n", config.NameForID(cur.ID), cur.Data["template"], serial)
	fmt.Fprintf(out, "new charger:     %s (template %s)\n", config.NameForID(easeeCharger.ID), easeeTemplate)

	if !apply {
		fmt.Fprintln(out, "dry run, use --apply to execute")
		return nil
	}

	if err := backupDatabase(out); err != nil {
		return err
	}

	if err := setLoadpointCharger(lp, config.NameForID(easeeCharger.ID)); err != nil {
		return err
	}

	user, password := dataString(easeeCharger.Data, "user"), dataString(easeeCharger.Data, "password")
	if err := easeeSetLocalOcpp(user, password, serial, "", false); err != nil {
		fmt.Fprintf(out, "warning: disabling local ocpp at easee failed, loadpoint was switched anyway: %v\n", err)
		return nil
	}

	fmt.Fprintf(out, "loadpoint %q now uses %s, local ocpp disabled at easee.\n", title, config.NameForID(easeeCharger.ID))
	return nil
}

// migrateLoadpoint returns the database loadpoint with the given title
func migrateLoadpoint(title string) (config.Config, error) {
	lps, err := config.ConfigurationsByClass(templates.Loadpoint)
	if err != nil {
		return config.Config{}, err
	}

	var titles []string
	for _, lp := range lps {
		t := dataString(lp.Data, "title")
		if t == title {
			return lp, nil
		}
		titles = append(titles, t)
	}

	return config.Config{}, fmt.Errorf("loadpoint %q not found in database (yaml loadpoints are not supported), available: %s", title, strings.Join(titles, ", "))
}

func loadpointCharger(lp config.Config) (config.Config, error) {
	ref := dataString(lp.Data, "charger")
	if !strings.HasPrefix(ref, "db:") {
		return config.Config{}, fmt.Errorf("loadpoint charger %q is not a database charger", ref)
	}

	id, err := config.IDForName(ref)
	if err != nil {
		return config.Config{}, err
	}

	return config.ConfigByID(id)
}

func setLoadpointCharger(lp config.Config, ref string) error {
	data := make(map[string]any, len(lp.Data))
	for k, v := range lp.Data {
		data[k] = v
	}
	data["charger"] = ref

	return lp.Update(data)
}

func findOcppCharger(tpl, serial string) (*config.Config, error) {
	chargers, err := config.ConfigurationsByClass(templates.Charger)
	if err != nil {
		return nil, err
	}

	for _, c := range chargers {
		if c.Data["template"] == tpl && dataString(c.Data, "stationid") == serial {
			return &c, nil
		}
	}

	return nil, nil
}

// ensureNoOpenSession aborts if a session of the loadpoint is still open
func ensureNoOpenSession(title string) error {
	var count int64
	err := db.Instance.Model(&session.Session{}).
		Where(map[string]any{"finished": "0001-01-01 00:00:00+00:00", "Loadpoint": title}).
		Count(&count).Error
	if err != nil {
		return err
	}

	if count > 0 {
		return fmt.Errorf("loadpoint %q has an open charging session, stop charging and evcc first", title)
	}

	return nil
}

// backupDatabase writes a timestamped copy next to the database. Replaced in tests.
var backupDatabase = func(out io.Writer) error {
	target := fmt.Sprintf("%s.%s.bak", db.FilePath(), time.Now().Format("20060102-150405"))
	if err := db.Backup(context.Background(), target); err != nil {
		return fmt.Errorf("database backup: %w", err)
	}

	fmt.Fprintf(out, "database backup: %s\n", target)
	return nil
}

func dataString(data map[string]any, key string) string {
	if v, ok := data[key]; ok && v != nil {
		return fmt.Sprint(v)
	}
	return ""
}
