package cmd

import (
	"fmt"

	"github.com/daboluocc/bbclaw/adapter/internal/buildinfo"
	"github.com/spf13/cobra"
)

// NewRootCmd creates the root command
func NewRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bbclaw-adapter",
		Short: "BBClaw adapter service",
		Long:  "BBClaw adapter service for AI agent integration and voice processing",
		RunE:  runAdapter,
	}

	// Add subcommands
	cmd.AddCommand(NewDoctorCmd())
	cmd.AddCommand(NewVersionCmd())

	return cmd
}

// NewVersionCmd creates the version command
func NewVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println(buildinfo.String("bbclaw-adapter"))
		},
	}
}

// runAdapter is the default command (runs the adapter service)
func runAdapter(cmd *cobra.Command, args []string) error {
	// This will be called from main.go
	// The actual adapter logic stays in main.go's run() function
	return nil
}
