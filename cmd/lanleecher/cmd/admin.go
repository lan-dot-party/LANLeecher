package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/lan-dot-party/lanleecher/internal/auth"
	"github.com/lan-dot-party/lanleecher/internal/config"
	"github.com/lan-dot-party/lanleecher/internal/secret"
	"github.com/lan-dot-party/lanleecher/internal/snmp"
	"github.com/lan-dot-party/lanleecher/internal/store"
)

func init() {
	configCmd.AddCommand(configInitCmd)
	configInitCmd.Flags().Bool("force", false, "overwrite an existing config.yaml")
	rootCmd.AddCommand(configCmd, migrateCmd, backupCmd, userCmd, eventCmd, switchCmd)

	userCreateCmd.Flags().String("role", store.RoleViewer, "viewer, operator or admin")
	userCreateCmd.Flags().String("language", "en", "en or de")
	userCmd.AddCommand(userCreateCmd, userPasswdCmd, userListCmd)

	eventStartCmd.Flags().String("notes", "", "optional notes")
	eventScheduleCmd.Flags().String("start", "", "first day, YYYY-MM-DD (required)")
	eventScheduleCmd.Flags().String("end", "", "last day, inclusive, YYYY-MM-DD (required)")
	eventScheduleCmd.Flags().String("start-time", "", "start time HH:MM on the first day (default 00:00)")
	eventScheduleCmd.Flags().String("end-time", "", "end time HH:MM on the last day (default 24:00)")
	eventScheduleCmd.Flags().String("notes", "", "optional notes")
	eventSchedulesCmd.Flags().Bool("all", false, "include schedules whose event has ended")
	eventCmd.AddCommand(eventStartCmd, eventStopCmd, eventListCmd, eventScheduleCmd, eventSchedulesCmd, eventUnscheduleCmd)

	switchAddCmd.Flags().String("name", "", "display name (required)")
	switchAddCmd.Flags().String("host", "", "IP or hostname (required)")
	switchAddCmd.Flags().Int("port", 161, "SNMP port")
	switchAddCmd.Flags().Bool("arp", false, "read the ARP table of this device for IP lookup")
	switchAddCmd.Flags().Bool("test", true, "test the connection before saving")
	switchCmd.AddCommand(switchAddCmd, switchListCmd)
}

var configCmd = &cobra.Command{Use: "config", Short: "Configuration helpers"}

var configInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Write an example config.yaml and create secret.key",
	RunE: func(cmd *cobra.Command, args []string) error {
		force, _ := cmd.Flags().GetBool("force")
		if _, err := os.Stat(configPath); err == nil && !force {
			return fmt.Errorf("%s exists (use --force to overwrite)", configPath)
		}
		if err := os.MkdirAll(filepath.Dir(configPath), 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(configPath, []byte(config.Example), 0o640); err != nil {
			return err
		}
		fmt.Println("wrote", configPath)
		cfg, err := config.Load(configPath)
		if err != nil {
			return err
		}
		keyPath := cfg.ResolvePath(cfg.SecretKeyFile)
		if _, err := os.Stat(keyPath); errors.Is(err, os.ErrNotExist) {
			if err := secret.Generate(keyPath); err != nil {
				return err
			}
			fmt.Println("created", keyPath)
		}
		return nil
	},
}

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Apply database migrations (the server also does this on start)",
	RunE: func(cmd *cobra.Command, args []string) error {
		e, err := open(cmd.Context())
		if err != nil {
			return err
		}
		defer e.close()
		v, err := e.db.SchemaVersion(cmd.Context())
		fmt.Println("schema version", v)
		return err
	},
}

var backupCmd = &cobra.Command{
	Use:   "backup [file]",
	Short: "Write a consistent copy of the SQLite database while the server runs",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(configPath)
		if err != nil {
			return err
		}
		db, err := store.Open(store.Options{Path: cfg.ResolvePath(cfg.Storage.Path)})
		if err != nil {
			return err
		}
		defer db.Close()
		target := filepath.Join(filepath.Dir(cfg.ResolvePath(cfg.Storage.Path)), "backups",
			"lanleecher-"+time.Now().Format("2006-01-02-150405")+".db")
		if len(args) == 1 {
			target = args[0]
		}
		if err := db.Backup(cmd.Context(), target); err != nil {
			return err
		}
		fmt.Println("backup written to", target)
		return nil
	},
}

var userCmd = &cobra.Command{Use: "user", Short: "Manage web users"}

var userCreateCmd = &cobra.Command{
	Use:   "create <username>",
	Short: "Create a user (password from prompt or stdin)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		role, _ := cmd.Flags().GetString("role")
		lang, _ := cmd.Flags().GetString("language")
		e, err := open(cmd.Context())
		if err != nil {
			return err
		}
		defer e.close()
		pw, err := readSecret("Password for "+args[0]+": ", true)
		if err != nil {
			return err
		}
		hash, err := auth.HashPassword(pw)
		if err != nil {
			return err
		}
		u := &store.User{Username: args[0], PasswordHash: hash, Role: role, Language: lang}
		if err := e.db.CreateUser(cmd.Context(), u); err != nil {
			return err
		}
		e.audit(cmd.Context(), store.AuditEntry{Actor: "cli", Action: "user.create", Target: u.Username, Details: role})
		fmt.Printf("user %s created with role %s\n", u.Username, u.Role)
		return nil
	},
}

var userPasswdCmd = &cobra.Command{
	Use:   "passwd <username>",
	Short: "Set a new password (ends all sessions of the user)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		e, err := open(cmd.Context())
		if err != nil {
			return err
		}
		defer e.close()
		u, err := e.db.UserByName(cmd.Context(), args[0])
		if err != nil {
			return fmt.Errorf("user %s: %w", args[0], err)
		}
		pw, err := readSecret("New password: ", true)
		if err != nil {
			return err
		}
		hash, err := auth.HashPassword(pw)
		if err != nil {
			return err
		}
		if err := e.db.SetPassword(cmd.Context(), u.ID, hash); err != nil {
			return err
		}
		e.audit(cmd.Context(), store.AuditEntry{Actor: "cli", Action: "user.password", Target: u.Username})
		fmt.Println("password changed")
		return nil
	},
}

var userListCmd = &cobra.Command{
	Use:   "list",
	Short: "List users",
	RunE: func(cmd *cobra.Command, args []string) error {
		e, err := open(cmd.Context())
		if err != nil {
			return err
		}
		defer e.close()
		users, err := e.db.ListUsers(cmd.Context())
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "USER\tROLE\tDISABLED")
		for _, u := range users {
			fmt.Fprintf(tw, "%s\t%s\t%v\n", u.Username, u.Role, u.Disabled)
		}
		return tw.Flush()
	},
}

var eventCmd = &cobra.Command{Use: "event", Short: "Start, stop and plan events"}

var eventStartCmd = &cobra.Command{
	Use:   "start <name>",
	Short: "Start an event (ends the running one)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		notes, _ := cmd.Flags().GetString("notes")
		e, err := open(cmd.Context())
		if err != nil {
			return err
		}
		defer e.close()
		ev, err := e.db.StartEvent(cmd.Context(), args[0], notes)
		if err != nil {
			return err
		}
		e.audit(cmd.Context(), store.AuditEntry{Actor: "cli", Action: "event.start", Target: ev.Slug, Details: ev.Name})
		fmt.Printf("event %q started (%s)\n", ev.Name, ev.Slug)
		return nil
	},
}

var eventStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the running event",
	RunE: func(cmd *cobra.Command, args []string) error {
		e, err := open(cmd.Context())
		if err != nil {
			return err
		}
		defer e.close()
		ev, err := e.db.StopEvent(cmd.Context())
		if errors.Is(err, store.ErrNotFound) {
			return errors.New("no event is running")
		}
		if err != nil {
			return err
		}
		e.audit(cmd.Context(), store.AuditEntry{Actor: "cli", Action: "event.stop", Target: ev.Slug})
		fmt.Printf("event %q stopped\n", ev.Name)
		return nil
	},
}

var eventListCmd = &cobra.Command{
	Use:   "list",
	Short: "List events",
	RunE: func(cmd *cobra.Command, args []string) error {
		e, err := open(cmd.Context())
		if err != nil {
			return err
		}
		defer e.close()
		events, err := e.db.ListEvents(cmd.Context(), "", time.Time{}, time.Time{})
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "SLUG\tNAME\tSTARTED\tENDED")
		for _, ev := range events {
			ended := "running"
			if ev.EndedAt != nil {
				ended = ev.EndedAt.Local().Format(time.DateTime)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", ev.Slug, ev.Name, ev.StartedAt.Local().Format(time.DateTime), ended)
		}
		return tw.Flush()
	},
}

var eventScheduleCmd = &cobra.Command{
	Use:   "schedule <name>",
	Short: "Plan an event that starts and stops by itself (dates in the local time zone)",
	Example: `  lanleecher event schedule "Basement LAN #13" --start 2027-04-15 --end 2027-04-18
  lanleecher event schedule "Basement LAN #13" --start 2027-04-15 --start-time 18:00 --end 2027-04-18 --end-time 14:00`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		f := cmd.Flags()
		startDate, _ := f.GetString("start")
		endDate, _ := f.GetString("end")
		startTime, _ := f.GetString("start-time")
		endTime, _ := f.GetString("end-time")
		notes, _ := f.GetString("notes")
		start, end, ok := store.ParseScheduleWindow(startDate, startTime, endDate, endTime, time.Local)
		if !ok {
			return errors.New("need --start and --end as YYYY-MM-DD (times as HH:MM), with the end after the start")
		}
		e, err := open(cmd.Context())
		if err != nil {
			return err
		}
		defer e.close()
		s := &store.EventSchedule{Name: args[0], Notes: notes, StartsAt: start, EndsAt: end}
		if err := e.db.CreateEventSchedule(cmd.Context(), s); err != nil {
			return err
		}
		e.audit(cmd.Context(), store.AuditEntry{Actor: "cli", Action: "schedule.create", Target: strconv.FormatInt(s.ID, 10),
			Details: s.Name})
		fmt.Printf("event %q planned (#%d): %s – %s\n", s.Name, s.ID,
			start.Local().Format(time.DateTime), end.Local().Format(time.DateTime))
		return nil
	},
}

var eventSchedulesCmd = &cobra.Command{
	Use:   "schedules",
	Short: "List planned events",
	RunE: func(cmd *cobra.Command, args []string) error {
		all, _ := cmd.Flags().GetBool("all")
		e, err := open(cmd.Context())
		if err != nil {
			return err
		}
		defer e.close()
		list, err := e.db.ListEventSchedules(cmd.Context(), all)
		if err != nil {
			return err
		}
		now := store.Now()
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tSTARTS\tENDS\tSTATE\tEVENT")
		for _, s := range list {
			slug := "-"
			if s.Event != nil {
				slug = s.Event.Slug
			}
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", s.ID, s.Name, s.StartsAt.Local().Format(time.DateTime),
				s.EndsAt.Local().Format(time.DateTime), s.State(now), slug)
		}
		return tw.Flush()
	},
}

var eventUnscheduleCmd = &cobra.Command{
	Use:   "unschedule <id>",
	Short: "Delete a planned event (an event it already started keeps running)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return errors.New("id must be a number, see 'event schedules'")
		}
		e, err := open(cmd.Context())
		if err != nil {
			return err
		}
		defer e.close()
		if err := e.db.DeleteEventSchedule(cmd.Context(), id); errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("no planned event #%d", id)
		} else if err != nil {
			return err
		}
		e.audit(cmd.Context(), store.AuditEntry{Actor: "cli", Action: "schedule.delete", Target: args[0]})
		fmt.Printf("planned event #%d deleted\n", id)
		return nil
	},
}

var switchCmd = &cobra.Command{Use: "switch", Short: "Manage switches (usually done in the web UI)"}

var switchAddCmd = &cobra.Command{
	Use:   "add",
	Short: "Add an SNMP v2c switch; the community is read from LANLEECHER_SNMP_COMMUNITY or stdin",
	RunE: func(cmd *cobra.Command, args []string) error {
		name, _ := cmd.Flags().GetString("name")
		host, _ := cmd.Flags().GetString("host")
		port, _ := cmd.Flags().GetInt("port")
		arp, _ := cmd.Flags().GetBool("arp")
		test, _ := cmd.Flags().GetBool("test")
		if name == "" || host == "" {
			return errors.New("--name and --host are required")
		}
		community := os.Getenv("LANLEECHER_SNMP_COMMUNITY")
		if community == "" {
			var err error
			if community, err = readSecret("Community: ", false); err != nil {
				return err
			}
		}
		creds := snmp.Credentials{Community: strings.TrimSpace(community)}
		e, err := open(cmd.Context())
		if err != nil {
			return err
		}
		defer e.close()
		target := snmp.Target{Host: host, Port: port, Version: "v2c", Creds: creds, Timeout: e.cfg.Polling.Timeout,
			Retries: e.cfg.Polling.Retries}
		if test {
			ctx, cancel := context.WithTimeout(cmd.Context(), 20*time.Second)
			defer cancel()
			g, err := snmp.Dial(ctx, target, 0)
			if err != nil {
				return err
			}
			res, err := g.Get([]string{"1.3.6.1.2.1.1.5.0"})
			snmp.Close(g)
			if err != nil {
				return fmt.Errorf("connection test failed: %w", err)
			}
			fmt.Println("connection ok, sysName:", snmp.String(res.Variables[0]))
		}
		plain, _ := creds.Marshal()
		enc, err := e.key.Encrypt(plain)
		if err != nil {
			return err
		}
		sw := &store.Switch{Name: name, Host: host, Port: port, SNMPVersion: "v2c", CredentialsEnc: enc,
			FDBAdapter: "auto", ARPSource: arp, Enabled: true}
		if err := e.db.CreateSwitch(cmd.Context(), sw); err != nil {
			return err
		}
		e.audit(cmd.Context(), store.AuditEntry{Actor: "cli", Action: "switch.create", Target: name, Details: host + " v2c"})
		fmt.Printf("switch %s added (id %d); a running server picks it up on SIGHUP or restart\n", name, sw.ID)
		return nil
	},
}

var switchListCmd = &cobra.Command{
	Use:   "list",
	Short: "List switches",
	RunE: func(cmd *cobra.Command, args []string) error {
		e, err := open(cmd.Context())
		if err != nil {
			return err
		}
		defer e.close()
		list, err := e.db.ListSwitches(cmd.Context())
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tHOST\tSNMP\tENABLED\tLAST POLL OK")
		for _, s := range list {
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%v\t%v\n", s.ID, s.Name, s.Host, s.SNMPVersion, s.Enabled, s.LastPollOK)
		}
		return tw.Flush()
	},
}
