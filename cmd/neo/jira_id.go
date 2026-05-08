package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ensamblatec/neoanvil/pkg/auth"
	"github.com/ensamblatec/neoanvil/pkg/jira"
)

// jiraIDCmd implements `neo jira-id <epic_id>` — resolves a master_plan
// epic ID (e.g. "130", "134.A.1") to its Jira ticket key (e.g. "MCPI-52)
// using pkg/jira.ResolveMasterPlanID. Prints the key on stdout for easy
// shell composition, e.g.:
//
//	git commit -m "feat: $(neo jira-id 134.A) — wraps phase A"
//
// [Épica 134.B.4]
func jiraIDCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "jira-id <epic_id>",
		Short: "Resolve a master_plan epic ID to its Jira ticket key (MCPI-N)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			epicID := args[0]

			creds, err := auth.Load(auth.DefaultCredentialsPath())
			if err != nil {
				return fmt.Errorf("load credentials: %w (run `neo login --provider jira`)", err)
			}
			entry := creds.GetByProvider("jira")
			if entry == nil {
				return errors.New("no jira credentials — run `neo login --provider jira`")
			}

			client, err := jira.NewClient(jira.Config{
				Domain: entry.Domain,
				Email:  entry.Email,
				Token:  entry.Token,
			})
			if err != nil {
				return fmt.Errorf("jira client: %w", err)
			}

			if project == "" {
				if store, err := auth.LoadContexts(auth.DefaultContextsPath()); err == nil {
					if active := store.ActiveSpace("jira"); active != nil {
						project = active.SpaceID
					}
				}
			}
			if project == "" {
				return errors.New("--project is required (or set active space via `neo space use --provider jira ...`)")
			}

			key, err := jira.ResolveMasterPlanID(context.Background(), client, project, epicID, nil)
			if errors.Is(err, jira.ErrAmbiguous) {
				fmt.Fprintln(cmd.OutOrStdout(), key)
				fmt.Fprintln(cmd.ErrOrStderr(), err)
				return nil
			}
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), key)
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Jira project key (default: active space from `neo space use`)")
	return cmd
}
