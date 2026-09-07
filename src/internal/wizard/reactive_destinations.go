package wizard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/darkharrbor/darkharrbor/internal/config"
	"github.com/darkharrbor/darkharrbor/internal/store"
)

type arrDestinationStore interface {
	ListArrInstances(context.Context) ([]store.ArrInstance, error)
	SetArrInstanceDestination(context.Context, string, string, int) error
}

type stagedArrDestination struct {
	name           string
	rootFolder     string
	qualityProfile int
}

type stagedArrDestinationStore struct {
	base    arrDestinationStore
	pending []stagedArrDestination
}

func newStagedArrDestinationStore(base arrDestinationStore) *stagedArrDestinationStore {
	return &stagedArrDestinationStore{base: base}
}

func (s *stagedArrDestinationStore) ListArrInstances(ctx context.Context) ([]store.ArrInstance, error) {
	if s == nil || s.base == nil {
		return nil, fmt.Errorf("reactive destination storage is unavailable")
	}
	return s.base.ListArrInstances(ctx)
}

func (s *stagedArrDestinationStore) SetArrInstanceDestination(_ context.Context, name, rootFolder string, qualityProfile int) error {
	if s == nil || s.base == nil {
		return fmt.Errorf("reactive destination storage is unavailable")
	}
	for i := range s.pending {
		if s.pending[i].name == name {
			s.pending[i] = stagedArrDestination{name: name, rootFolder: rootFolder, qualityProfile: qualityProfile}
			return nil
		}
	}
	s.pending = append(s.pending, stagedArrDestination{name: name, rootFolder: rootFolder, qualityProfile: qualityProfile})
	return nil
}

func (s *stagedArrDestinationStore) Apply() error {
	if s == nil || s.base == nil {
		return fmt.Errorf("reactive destination storage is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, choice := range s.pending {
		if err := s.base.SetArrInstanceDestination(ctx, choice.name, choice.rootFolder, choice.qualityProfile); err != nil {
			return err
		}
	}
	return nil
}

type arrRootFolder struct {
	ID   int    `json:"id"`
	Path string `json:"path"`
}

type arrQualityProfile struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

func captureReactiveDestinations(p *configurePrompter, cfg *config.Config, values map[string]string, destinations arrDestinationStore) error {
	if destinations == nil {
		return fmt.Errorf("reactive destination storage is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	instances, err := destinations.ListArrInstances(ctx)
	if err != nil {
		return fmt.Errorf("list reactive Arr destinations: %w", err)
	}
	targets := make(map[string]config.ArrTarget, len(cfg.Arrs))
	for _, target := range cfg.Arrs {
		targets[target.Name] = target
	}
	var seriesNames, movieNames []string
	for _, instance := range instances {
		if instance.AppType != "sonarr" && instance.AppType != "radarr" {
			continue
		}
		target, ok := targets[instance.Name]
		if !ok || strings.TrimSpace(target.BaseURL) == "" || strings.TrimSpace(target.APIKey) == "" {
			return fmt.Errorf("reactive destination %s has no resolved runtime credentials", instance.Name)
		}
		roots, profiles, err := fetchArrDestinationChoices(ctx, target)
		if err != nil {
			return fmt.Errorf("reactive destination %s: %w", instance.Name, err)
		}
		root, err := chooseArrRoot(p, instance.Name, roots, instance.RootFolder)
		if err != nil {
			return err
		}
		profile, err := chooseArrProfile(p, instance.Name, profiles, instance.QualityProfile)
		if err != nil {
			return err
		}
		if err := destinations.SetArrInstanceDestination(ctx, instance.Name, root.Path, profile.ID); err != nil {
			return err
		}
		fmt.Fprintf(p.out, "  %s destination: root %s, quality profile %d (%s)\n", instance.Name, root.Path, profile.ID, profile.Name)
		if instance.AppType == "sonarr" {
			seriesNames = append(seriesNames, instance.Name)
		} else {
			movieNames = append(movieNames, instance.Name)
		}
	}
	sort.Strings(seriesNames)
	sort.Strings(movieNames)
	seriesDefault, err := chooseOptionalArrDefault(p, "Default series Arr", seriesNames, currentTuningValue(cfg, values, "HARRBOR_REACTIVE_DEFAULT_SERIES_ARR"))
	if err != nil {
		return err
	}
	movieDefault, err := chooseOptionalArrDefault(p, "Default movie Arr", movieNames, currentTuningValue(cfg, values, "HARRBOR_REACTIVE_DEFAULT_MOVIE_ARR"))
	if err != nil {
		return err
	}
	if seriesDefault == "" {
		delete(values, "HARRBOR_REACTIVE_DEFAULT_SERIES_ARR")
	} else {
		values["HARRBOR_REACTIVE_DEFAULT_SERIES_ARR"] = seriesDefault
	}
	if movieDefault == "" {
		delete(values, "HARRBOR_REACTIVE_DEFAULT_MOVIE_ARR")
	} else {
		values["HARRBOR_REACTIVE_DEFAULT_MOVIE_ARR"] = movieDefault
	}
	return nil
}

func fetchArrDestinationChoices(ctx context.Context, target config.ArrTarget) ([]arrRootFolder, []arrQualityProfile, error) {
	var roots []arrRootFolder
	if err := getArrJSON(ctx, target, "/api/v3/rootfolder", &roots); err != nil {
		return nil, nil, fmt.Errorf("fetch root folders: %w", err)
	}
	var profiles []arrQualityProfile
	if err := getArrJSON(ctx, target, "/api/v3/qualityprofile", &profiles); err != nil {
		return nil, nil, fmt.Errorf("fetch quality profiles: %w", err)
	}
	validRoots := roots[:0]
	for _, root := range roots {
		root.Path = strings.TrimSpace(root.Path)
		if root.ID > 0 && root.Path != "" {
			validRoots = append(validRoots, root)
		}
	}
	validProfiles := profiles[:0]
	for _, profile := range profiles {
		profile.Name = strings.TrimSpace(profile.Name)
		if profile.ID > 0 && profile.Name != "" {
			validProfiles = append(validProfiles, profile)
		}
	}
	if len(validRoots) == 0 || len(validProfiles) == 0 {
		return nil, nil, fmt.Errorf("arr returned no usable destination choices")
	}
	return validRoots, validProfiles, nil
}

func getArrJSON(ctx context.Context, target config.ArrTarget, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(target.BaseURL, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", target.APIKey)
	resp, err := (&http.Client{Timeout: 8 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("arr returned HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode Arr response: %w", err)
	}
	return nil
}

func chooseArrRoot(p *configurePrompter, name string, roots []arrRootFolder, current string) (arrRootFolder, error) {
	if len(roots) == 1 {
		fmt.Fprintf(p.out, "  %s root folder: %s (only available root; selected automatically)\n", name, roots[0].Path)
		return roots[0], nil
	}
	fmt.Fprintf(p.out, "  %s root folders:\n", name)
	def := 1
	for i, root := range roots {
		fmt.Fprintf(p.out, "    %d) %s\n", i+1, root.Path)
		if root.Path == current {
			def = i + 1
		}
	}
	choice, _, err := p.integer("Select "+name+" root folder number", def, 1)
	if err != nil || choice > len(roots) {
		return arrRootFolder{}, fmt.Errorf("%s root folder selection must be from 1 through %d", name, len(roots))
	}
	return roots[choice-1], nil
}

func chooseArrProfile(p *configurePrompter, name string, profiles []arrQualityProfile, current int) (arrQualityProfile, error) {
	if len(profiles) == 1 {
		fmt.Fprintf(p.out, "  %s quality profile: %d (%s) (only available profile; selected automatically)\n", name, profiles[0].ID, profiles[0].Name)
		return profiles[0], nil
	}
	fmt.Fprintf(p.out, "  %s quality profiles:\n", name)
	def := 1
	for i, profile := range profiles {
		fmt.Fprintf(p.out, "    %d) %d (%s)\n", i+1, profile.ID, profile.Name)
		if profile.ID == current {
			def = i + 1
		}
	}
	choice, _, err := p.integer("Select "+name+" quality profile number", def, 1)
	if err != nil || choice > len(profiles) {
		return arrQualityProfile{}, fmt.Errorf("%s quality profile selection must be from 1 through %d", name, len(profiles))
	}
	return profiles[choice-1], nil
}

func chooseOptionalArrDefault(p *configurePrompter, label string, names []string, current string) (string, error) {
	if len(names) == 0 {
		return "", nil
	}
	fmt.Fprintf(p.out, "  %s choices:\n    0) none\n", label)
	def := 0
	for i, name := range names {
		fmt.Fprintf(p.out, "    %d) %s\n", i+1, name)
		if name == current {
			def = i + 1
		}
	}
	choice, _, err := p.integer(label+" number", def, 0)
	if err != nil || choice > len(names) {
		return "", fmt.Errorf("%s selection must be from 0 through %d", label, len(names))
	}
	if choice == 0 {
		return "", nil
	}
	return names[choice-1], nil
}

func currentTuningValue(cfg *config.Config, values map[string]string, key string) string {
	if value, ok := values[key]; ok {
		return strings.TrimSpace(value)
	}
	value, _ := cfg.Lookup(key)
	return strings.TrimSpace(value)
}
