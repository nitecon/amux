package amuxruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/nitecon/amux/worksubmission"
	"github.com/urfave/cli/v3"
)

func runTaskReport(ctx context.Context, args []string, jsonOutput bool, serverURL, project, apiKey, enrolledProfile string, stdout, stderr io.Writer) error {
	flags := commandFlags("tasks report", stderr)
	rangeValue := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "range", Value: "", Usage: "Git commit range FROM..TO", Destination: rangeValue})
	since := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "since", Value: "", Usage: "inclusive start date (YYYY-MM-DD or RFC3339)", Destination: since})
	until := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "until", Value: "", Usage: "exclusive end date (YYYY-MM-DD or RFC3339)", Destination: until})
	offline := new(bool)
	flags.Flags = append(flags.Flags, &cli.BoolFlag{Name: "offline", Value: false, Usage: "use local Git data only", Destination: offline})
	format := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "format", Value: "human", Usage: "output format: human or json", Destination: format})
	pos, err := parseOperation(ctx, flags, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return fmt.Errorf("tasks report accepts at most one positional range")
	}
	if len(pos) == 1 {
		if *rangeValue != "" {
			return fmt.Errorf("provide the range either positionally or with --range")
		}
		*rangeValue = pos[0]
	}
	if *format != "human" && *format != "json" {
		return fmt.Errorf("--format must be human or json")
	}
	root, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve current checkout: %w", err)
	}
	report, err := worksubmission.BuildLocalReport(ctx, root, worksubmission.ReportOptions{Range: *rangeValue, Since: *since, Until: *until})
	if err != nil {
		return err
	}
	if *offline {
		report.Partial = append([]string{"Offline: gateway tasks/reviews and GitHub CI not requested"}, report.Partial...)
	} else {
		if project == "" || (enrolledProfile == "" && (serverURL == "" || apiKey == "")) {
			report.Partial = append(report.Partial, "Gateway task/review facts unavailable: no enrolled read-only Gateway connection and project supplied")
		} else if client, _, clientErr := gatewayClientForProfile(serverURL, enrolledProfile, apiKey); clientErr != nil {
			report.Partial = append(report.Partial, fmt.Sprintf("Gateway task/review facts unavailable: %v", clientErr))
		} else {
			gatewayCtx, cancel := context.WithTimeout(ctx, time.Minute)
			if enrichErr := report.EnrichGateway(gatewayCtx, client, project); enrichErr != nil {
				report.Partial = append(report.Partial, fmt.Sprintf("Gateway task/review facts unavailable: %v", enrichErr))
			}
			cancel()
		}
		ciRuns, ciErr := worksubmission.ReadReportCI(ctx, root, report.ToSHA)
		if ciErr != nil {
			report.Partial = append(report.Partial, fmt.Sprintf("Exact-SHA GitHub CI facts unavailable: %v", ciErr))
		} else {
			report.CIRuns = ciRuns
		}
	}
	if jsonOutput || *format == "json" {
		return json.NewEncoder(stdout).Encode(report)
	}
	_, err = fmt.Fprintln(stdout, report.Render())
	return err
}
