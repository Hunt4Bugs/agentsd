package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Hunt4Bugs/agentsd/internal/doctor"
	"github.com/Hunt4Bugs/agentsd/internal/exitcode"
)

func (a *App) doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check environment, paths, runtimes, and daemon",
		Args:  args(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			rep := doctor.Run(cmd.Context(), a.env)
			if a.JSON {
				a.writeJSON(rep)
			} else {
				tw := a.table()
				for _, c := range rep.Checks {
					if a.Quiet && c.Status == doctor.OK {
						continue
					}
					st := string(c.Status)
					switch c.Status {
					case doctor.OK:
						st = a.paint("32", st)
					case doctor.Warn:
						st = a.paint("33", st)
					case doctor.Fail:
						st = a.paint("31", st)
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\n", st, c.Name, c.Detail)
				}
				tw.Flush()
			}
			if !rep.OK {
				return &exitcode.Silent{Code: exitcode.Invalid}
			}
			return nil
		},
	}
}
