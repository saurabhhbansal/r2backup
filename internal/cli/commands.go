package cli

import (
	"github.com/spf13/cobra"
)

func newAddCmd(opts *Options) *cobra.Command {
	var (
		interval  int
		retention int
		all       bool
	)
	cmd := &cobra.Command{
		Use:   "add <folder>",
		Short: "Pick what to include in a folder, then back it up",
		Long: "Opens the folder as a tree with everything already selected.\n" +
			"Uncheck what you do not want and press enter; press enter straight\n" +
			"away and all of it goes. This is the only place choices are made.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return notYet("add") },
	}
	cmd.Flags().IntVar(&interval, "every", 0,
		"minutes between scheduled runs (default 30)")
	cmd.Flags().IntVar(&retention, "retention", -1,
		"days to keep deleted and overwritten files; 0 disables trash (default 30)")
	cmd.Flags().BoolVar(&all, "all", false,
		"skip the picker and include everything")
	return cmd
}

func newBackupCmd(opts *Options) *cobra.Command {
	var verify bool
	cmd := &cobra.Command{
		Use:   "backup [set]",
		Short: "Back up now, all sets or one",
		Args:  cobra.MaximumNArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return notYet("backup") },
	}
	cmd.Flags().BoolVar(&verify, "verify", false,
		"hash file contents instead of trusting size and modification time")
	return cmd
}

func newRestoreCmd(opts *Options) *cobra.Command {
	var (
		to        string
		only      string
		machine   string
		overwrite bool
		verify    bool
		deleted   string
	)
	cmd := &cobra.Command{
		Use:   "restore <set>",
		Short: "Bring a folder back",
		Long: "Restores to the folder's original path when that path exists on this\n" +
			"machine, and otherwise requires --to. It never guesses and never asks.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return notYet("restore") },
	}
	cmd.Flags().StringVar(&to, "to", "", "restore into this directory instead")
	cmd.Flags().StringVar(&only, "only", "", "restore only paths matching this glob")
	cmd.Flags().StringVar(&machine, "machine", "", "restore from another computer's backup")
	cmd.Flags().StringVar(&deleted, "deleted", "", "recover a deleted file from trash")
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "replace files that already exist")
	cmd.Flags().BoolVar(&verify, "verify", false, "re-download and byte-compare afterwards")
	return cmd
}

func newStatusCmd(opts *Options) *cobra.Command {
	var watch bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "What ran, when, and what is next",
		RunE:  func(cmd *cobra.Command, args []string) error { return notYet("status") },
	}
	cmd.Flags().BoolVar(&watch, "watch", false,
		"follow a run already in progress, including one started by the scheduler")
	return cmd
}

func newListCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "ls [set]",
		Short: "What is in the backup",
		Args:  cobra.MaximumNArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return notYet("ls") },
	}
}

func newScheduleCmd(opts *Options) *cobra.Command {
	var (
		every  int
		remove bool
	)
	cmd := &cobra.Command{
		Use:   "schedule",
		Short: "Register with the operating system's scheduler",
		Long: "Registers a hidden Task Scheduler entry on Windows, a systemd user\n" +
			"timer on Linux, or a launchd agent on macOS. Nothing of ours stays\n" +
			"running in between, so updating the binary is never blocked.",
		RunE: func(cmd *cobra.Command, args []string) error { return notYet("schedule") },
	}
	cmd.Flags().IntVar(&every, "every", 30, "minutes between runs")
	cmd.Flags().BoolVar(&remove, "remove", false, "unregister instead")
	return cmd
}

func newRenameCmd(opts *Options) *cobra.Command {
	var remote bool
	cmd := &cobra.Command{
		Use:   "rename <set> <new-name>",
		Short: "Change what a set is called",
		Long: "Changes the name only. The bucket keeps the prefix it was created\n" +
			"with, so this is instant and costs nothing. Pass --remote to move the\n" +
			"objects too; it will tell you how many operations that takes first.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error { return notYet("rename") },
	}
	cmd.Flags().BoolVar(&remote, "remote", false,
		"also move the objects in the bucket, at one operation each")
	return cmd
}

func newRelinkCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "relink <set> <new-path>",
		Short: "Point a set at a folder that moved",
		Long: "Use this when a backed-up folder was renamed or moved. Nothing is\n" +
			"re-uploaded: the objects in the bucket are still correct, only the\n" +
			"local path changed.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error { return notYet("relink") },
	}
}

func newTrashCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trash",
		Short: "What is recoverable, and until when",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "ls [set]",
		Short: "List recoverable files and their expiry",
		Args:  cobra.MaximumNArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return notYet("trash ls") },
	})
	return cmd
}
