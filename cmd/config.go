package cmd

import (
	"fmt"
	"maps"
	"slices"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/output"
)

// configView is the machine-readable shape of `config show`. It deliberately has no
// field for the API key — only a description of which credential type is in play.
type configView struct {
	Profile          string `json:"profile"`
	ConfigFile       string `json:"config_file"`
	APIURL           string `json:"api_url"`
	AuthURL          string `json:"auth_url"`
	AppURL           string `json:"app_url"`
	OTLPURL          string `json:"otlp_url"`
	OrganizationID   string `json:"organization_id,omitempty"`
	OrganizationName string `json:"organization_name,omitempty"`
	ProjectID        string `json:"project_id,omitempty"`
	ProjectName      string `json:"project_name,omitempty"`
	Auth             string `json:"auth"`
}

func newConfigCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "config",
		Short:  "Inspect and switch configuration profiles",
		Hidden: true,
		Long: `Profiles keep separate selections side by side — two organizations you move
between, or two projects you compare.

Credentials are stored per profile too, so switching profiles switches identity.`,
	}
	cmd.AddCommand(newConfigShowCommand(), newConfigProfilesCommand(), newConfigUseCommand(), newConfigSetCommand())
	return cmd
}

func newConfigShowCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Show the resolved configuration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadProjectConfig()
			if err != nil {
				return err
			}
			format, err := resolveFormat()
			if err != nil {
				return err
			}
			path, err := config.ConfigPath()
			if err != nil {
				return err
			}

			authMode := "cli token"
			if cfg.APIKey != "" {
				authMode = "server key (TERMA_API_KEY)"
			}

			// A purpose-built struct rather than cfg: the machine-readable view must
			// describe how the CLI is authenticating without ever emitting the
			// credential itself.
			view := configView{
				Profile:          cfg.ProfileName,
				ConfigFile:       path,
				APIURL:           cfg.APIURL,
				AuthURL:          cfg.AuthURL,
				AppURL:           cfg.AppURL,
				OTLPURL:          cfg.OTLPURL,
				OrganizationID:   cfg.OrganizationID,
				OrganizationName: cfg.OrganizationName,
				ProjectID:        cfg.ProjectID,
				ProjectName:      cfg.ProjectName,
				Auth:             authMode,
			}

			return output.KeyValues(cmd.OutOrStdout(), format, [][2]string{
				{"profile", cfg.ProfileName},
				{"config file", path},
				{"api url", cfg.APIURL},
				{"auth url", cfg.AuthURL},
				{"app url", cfg.AppURL},
				{"otlp url", cfg.OTLPURL},
				{"organization", nameOrID(cfg.OrganizationName, cfg.OrganizationID)},
				{"project", nameOrID(cfg.ProjectName, cfg.ProjectID)},
				{"auth", authMode},
			}, view)
		},
	}
}

func newConfigProfilesCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "profiles",
		Aliases: []string{"list"},
		Short:   "List configured profiles",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := resolveFormat()
			if err != nil {
				return err
			}
			file, err := config.LoadFile()
			if err != nil {
				return err
			}

			// By name: a map's order changes from one run to the next, and a list that
			// reshuffles itself reads as though something changed.
			rows := make([][]string, 0, len(file.Profiles))
			for _, name := range slices.Sorted(maps.Keys(file.Profiles)) {
				marker := " "
				if name == file.ActiveProfile {
					marker = "*"
				}
				rows = append(rows, []string{marker, name, nameOrID(file.Profiles[name].OrganizationName, file.Profiles[name].OrganizationID)})
			}

			return output.Render(cmd.OutOrStdout(), format, output.Table{
				Headers: []string{"", "PROFILE", "ORGANIZATION"},
				Rows:    rows,
			}, file)
		},
	}
}

func newConfigUseCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "use <profile>",
		Short: "Switch the active profile",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			file, err := config.LoadFile()
			if err != nil {
				return err
			}
			name := args[0]
			if _, ok := file.Profiles[name]; !ok {
				// Creating it implicitly is friendlier than erroring: `config set` and
				// `login` both work against a profile that does not exist yet.
				file.Profiles[name] = &config.Profile{}
			}
			file.ActiveProfile = name
			if err := config.SaveFile(file); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Active profile is now %s.\n", name)
			return nil
		},
	}
}

func newConfigSetCommand() *cobra.Command {
	var apiURL, authURL, appURL, otlpURL string

	cmd := &cobra.Command{
		Use:   "set",
		Short: "Point the active profile at a different deployment",
		Long: `Stores endpoint overrides on the active profile.

Defaults to Terma's production endpoints; set these only for a self-hosted
deployment. Combine with ` + "`terma config use <name>`" + ` to keep one profile per
deployment and switch between them.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if apiURL == "" && authURL == "" && appURL == "" && otlpURL == "" {
				return fmt.Errorf("nothing to set — pass --api-url, --auth-url, --app-url, or --otlp-url")
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if err := config.UpdateProfile(cfg.ProfileName, func(p *config.Profile) {
				if apiURL != "" {
					p.APIURL = apiURL
				}
				if authURL != "" {
					p.AuthURL = authURL
				}
				if appURL != "" {
					p.AppURL = appURL
				}
				if otlpURL != "" {
					p.OTLPURL = otlpURL
				}
			}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Updated profile %s.\n", cfg.ProfileName)
			return nil
		},
	}

	cmd.Flags().StringVar(&apiURL, "api-url", "", "Terma data API base URL")
	cmd.Flags().StringVar(&authURL, "auth-url", "", "Terma auth API base URL")
	cmd.Flags().StringVar(&appURL, "app-url", "", "Terma app base URL")
	cmd.Flags().StringVar(&otlpURL, "otlp-url", "", "Terma OTLP ingest URL (written into harness configs by `telemetry connect`)")
	return cmd
}
