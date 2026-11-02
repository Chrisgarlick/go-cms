package cli

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/spf13/cobra"

	"cms/internal/tailwind"
)

func tailwindCmd() *cobra.Command {
	var check bool
	cmd := &cobra.Command{
		Use:   "tailwind [site-dir]",
		Short: "Build the theme's Tailwind CSS (theme/tailwind → theme/assets/tailwind.css)",
		Long: "Compiles theme/tailwind/input.css (with tailwind.config.js) into theme/assets/tailwind.css\n" +
			"using the standalone Tailwind CLI " + tailwind.Version + ", downloaded and checksum-verified on first\n" +
			"use. Commit the output: the server never builds CSS. --check fails if the committed\n" +
			"file is out of date (CI; cms check does this too). cms dev rebuilds as you edit.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			dir, _ = filepath.Abs(dir)
			if !tailwind.Used(dir) {
				return errors.New("no " + tailwind.Input + " in this site")
			}
			if check {
				if err := tailwind.Check(cmd.Context(), dir); err != nil {
					return err
				}
				return out(map[string]any{"ok": true}, func(w io.Writer) { fmt.Fprintln(w, tailwind.Output+" is up to date.") })
			}
			wrote, err := tailwind.Build(cmd.Context(), dir)
			if err != nil {
				return err
			}
			return out(map[string]any{"written": wrote}, func(w io.Writer) {
				if wrote {
					fmt.Fprintln(w, "Wrote "+tailwind.Output+".")
				} else {
					fmt.Fprintln(w, tailwind.Output+" is already up to date.")
				}
			})
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "fail if the committed CSS doesn't match its sources")
	return cmd
}
