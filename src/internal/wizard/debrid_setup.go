package wizard

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/alldebrid"
	"github.com/darkharrbor/darkharrbor/internal/premiumize"
	"github.com/darkharrbor/darkharrbor/internal/realdebrid"
)

type debridAnswers struct {
	RealDebrid string
	AllDebrid  string
	Premiumize string
}

func promptOtherDebridProviders(ctx context.Context, out io.Writer, selected []string) (debridAnswers, error) {
	if !containsProvider(selected, "realdebrid") && !containsProvider(selected, "alldebrid") && !containsProvider(selected, "premiumize") {
		fmt.Fprintln(out, "\n── S2b/S3/S3b Other Debrid Providers ─────────────────")
		fmt.Fprintln(out, "  · Skipped: no additional debrid provider was selected.")
		return debridAnswers{}, nil
	}
	var answers debridAnswers

	if containsProvider(selected, "realdebrid") {
		fmt.Fprintln(out, "\n── S2b Real-Debrid ────────────────────────────────────")
		rdToken, err := promptSecret(out, "  Real-Debrid API token: ")
		if err != nil {
			return answers, fmt.Errorf("S2b: read RD token: %w", err)
		}
		answers.RealDebrid = strings.TrimSpace(rdToken)
		if answers.RealDebrid == "" {
			return answers, fmt.Errorf("S2b: Real-Debrid was selected but no API token was supplied")
		}
		rdPolicy := realdebrid.NewRealDebridPolicy()
		rdClient := realdebrid.NewHTTPClient("", answers.RealDebrid, "darkharrbor-wizard/1", 10*time.Second, rdPolicy)
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		info, probeErr := rdClient.GetUser(probeCtx)
		cancel()
		if probeErr != nil {
			return answers, fmt.Errorf("S2b: account verification failed: %w", probeErr)
		}
		premium, _, _ := realdebrid.ResolveCaps(info)
		if !premium {
			return answers, fmt.Errorf("S2b: Real-Debrid account is not Premium")
		}
		fmt.Fprintf(out, "  ✓ Real-Debrid Premium account (%d days remaining)\n", info.ExpirationDays)
	}

	if containsProvider(selected, "alldebrid") {
		fmt.Fprintln(out, "\n── S3  AllDebrid ──────────────────────────────────────")
		adKey, err := promptSecret(out, "  AllDebrid API key: ")
		if err != nil {
			return answers, fmt.Errorf("S3: read AllDebrid key: %w", err)
		}
		answers.AllDebrid = strings.TrimSpace(adKey)
		if answers.AllDebrid == "" {
			return answers, fmt.Errorf("S3: AllDebrid was selected but no API key was supplied")
		}
		policy := alldebrid.NewPolicy()
		defer policy.Stop()
		client := alldebrid.NewHTTPClient("", answers.AllDebrid, "darkharrbor-wizard/1", 10*time.Second, policy)
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		info, probeErr := client.GetUser(probeCtx)
		cancel()
		if probeErr != nil {
			return answers, fmt.Errorf("S3: account verification failed: %w", probeErr)
		}
		if !info.Premium {
			return answers, fmt.Errorf("S3: AllDebrid account is not Premium")
		}
		fmt.Fprintln(out, "  ✓ AllDebrid Premium account")
	}

	if containsProvider(selected, "premiumize") {
		fmt.Fprintln(out, "\n── S3b Premiumize ─────────────────────────────────────")
		pmKey, err := promptSecret(out, "  Premiumize API key: ")
		if err != nil {
			return answers, fmt.Errorf("S3b: read Premiumize key: %w", err)
		}
		answers.Premiumize = strings.TrimSpace(pmKey)
		if answers.Premiumize == "" {
			return answers, fmt.Errorf("S3b: Premiumize was selected but no API key was supplied")
		}
		policy := premiumize.NewPolicy()
		defer policy.Stop()
		client := premiumize.NewHTTPClient("", answers.Premiumize, "darkharrbor-wizard/1", 10*time.Second, policy)
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		info, probeErr := client.GetAccount(probeCtx)
		cancel()
		if probeErr != nil {
			return answers, fmt.Errorf("S3b: account verification failed: %w", probeErr)
		}
		if !premiumize.PremiumAt(info, time.Now()) {
			return answers, fmt.Errorf("S3b: Premiumize account is not Premium")
		}
		fmt.Fprintf(out, "  ✓ Premiumize account (fair-use used %.1f%%)\n", info.LimitUsed*100)
	}

	return answers, nil
}

func containsProvider(providers []string, name string) bool {
	for _, provider := range providers {
		if provider == name {
			return true
		}
	}
	return false
}

func (a debridAnswers) any() bool {
	return a.RealDebrid != "" || a.AllDebrid != "" || a.Premiumize != ""
}
