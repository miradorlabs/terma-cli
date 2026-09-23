// funding-eval runs the estimator over simulated worlds and reports how far its
// figure lands from what the provider would have charged.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/miradorlabs/terma-cli/pocs/funding-model/eval"
)

func main() {
	scenario := flag.String("scenario", "all", "preset name, comma list, or all")
	seeds := flag.Int("seeds", 20, "seeds per scenario")
	asJSON := flag.Bool("json", false, "machine-readable output")
	check := flag.Bool("check", true, "fail when a scenario misses its case")
	perSeed := flag.Bool("per-seed", false, "also print every seed's row")
	breakdown := flag.Int64("breakdown", 0, "explain this seed per user (with -scenario NAME)")
	flag.Parse()

	if *breakdown > 0 {
		for _, sc := range eval.Scenarios(strings.Split(*scenario, ",")) {
			fmt.Printf("%s seed %d, second half per user:\n", sc.Name, *breakdown)
			for _, b := range eval.Breakdown(sc, *breakdown) {
				fmt.Printf("  %-5s calls %5d credits %5d ref %9.2f true %9.2f cold %9.2f warm %9.2f  fit scale %.2f hidden %.2f$/h (%.0f days) share %.2f route %v\n",
					b.UserID, b.Calls, b.CreditCalls, b.ReferenceUSD, b.TrueUSD, b.ColdUSD, b.WarmUSD,
					b.Prior.FillScale, b.Prior.HiddenUSDPerHour, b.Prior.FitDays,
					(b.Prior.CreditsA+0.5)/(b.Prior.CreditsA+b.Prior.CreditsB+2.5), b.Prior.RouteObs)
			}
		}
		return
	}

	scs := eval.Scenarios(strings.Split(*scenario, ","))
	if len(scs) == 0 {
		fmt.Fprintln(os.Stderr, "no such scenario")
		os.Exit(2)
	}
	var sums []eval.Summary
	failed := false
	for _, sc := range scs {
		sums = append(sums, eval.RunMany(sc, *seeds))
		if *perSeed && !*asJSON {
			for s := 1; s <= *seeds; s++ {
				m := eval.Run(sc, int64(s))
				fmt.Printf("%-28s seed %2d calls %4d ref %8.2f true %8.2f cold %7.2f (Δ %5.1f%%) acc %.2f warm2nd Δ %5.1f%% cold2nd Δ %5.1f%% blocked %d\n",
					sc.Name, s, m.Calls, m.ReferenceUSD, m.TrueMeteredUSD, m.ColdEstimatedUSD, 100*m.ColdDelta, m.ColdAccuracy,
					100*m.SecondWarmDelta, 100*m.SecondColdDelta, m.Blocked)
			}
		}
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(sums)
	} else {
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "scenario\tcalls\tref $\ttrue $\tnaive Δ\tcold Δ (p90)\tacc\tbrier\talloc err\twarm Δ 2nd (p90)\tcold Δ 2nd\tcase")
		for _, s := range sums {
			verdict := "-"
			if *check {
				if c, ok := eval.CaseFor(s.Scenario); ok {
					if fails := eval.Check(c, s); len(fails) > 0 {
						verdict = "FAIL: " + strings.Join(fails, "; ")
						failed = true
					} else {
						verdict = "ok"
					}
				}
			}
			m, w := s.Mean, s.Worst
			fmt.Fprintf(tw, "%s\t%d\t%.2f\t%.2f\t%.1f%%\t%.1f%% (%.1f%%)\t%.2f\t%.3f\t%.2f\t%.1f%% (%.1f%%)\t%.1f%%\t%s\n",
				s.Scenario, m.Calls, m.ReferenceUSD, m.TrueMeteredUSD, 100*m.NaiveDelta,
				100*m.ColdDelta, 100*w.ColdDelta, m.ColdAccuracy, m.ColdBrier, m.AllocationError,
				100*m.SecondWarmDelta, 100*w.SecondWarmDelta, 100*m.SecondColdDelta, verdict)
		}
		_ = tw.Flush()
		fmt.Println()
		fmt.Println("Δ = |shown − charged| / total reference cost. naive = today's list-price figure.")
		fmt.Println("cold = nothing learned; warm = second half after reconciling the first half's exports. (p90) = 90th percentile over seeds.")
	}
	if failed {
		os.Exit(1)
	}
}
