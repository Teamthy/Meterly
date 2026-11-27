// Command certify runs live-sandbox certification scenarios and writes
// certify-report.json. Each provider is skipped unless its keys are set.
//
//	go run ./cmd/certify -o certify-report.json
//
// Exit 1 only when a scenario with keys fails. Skipped (no keys) is success:
// those providers are recorded as not certified.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/teamthy/meterly/internal/certify"
)

func main() {
	out := flag.String("o", "certify-report.json", "report path (no secrets)")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	rep := certify.Run(ctx, certify.FromEnv())
	if err := certify.WriteFile(*out, rep); err != nil {
		fmt.Fprintf(os.Stderr, "certify: write %s: %v\n", *out, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "certify: wrote %s passed=%d failed=%d skipped=%d\n",
		*out, rep.Summary.Passed, rep.Summary.Failed, rep.Summary.Skipped)
	if rep.Summary.Failed > 0 {
		os.Exit(1)
	}
}
