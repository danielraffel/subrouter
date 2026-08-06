package azureopenai

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/storepath"
)

const (
	CognitiveServicesTokenResource = "https://cognitiveservices.azure.com"
	FoundryTokenResource           = "https://ai.azure.com"
)

var profileNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

type Profile struct {
	Name          string `json:"name"`
	Endpoint      string `json:"endpoint"`
	Deployment    string `json:"deployment"`
	TokenResource string `json:"tokenResource"`
	AzureCLI      string `json:"azureCli"`
	AddedAt       string `json:"addedAt"`
}

type Store struct {
	Path string
}

type profilesFile struct {
	Version  int       `json:"version"`
	Profiles []Profile `json:"profiles"`
}

func DefaultStore() Store {
	return Store{Path: filepath.Join(storepath.CodexDir(), "azure-openai.json")}
}

func (s Store) ProfilesPath() string {
	if strings.TrimSpace(s.Path) != "" {
		return s.Path
	}
	return DefaultStore().Path
}

func NormalizeProfile(profile Profile) (Profile, error) {
	profile.Name = strings.ToLower(strings.TrimSpace(profile.Name))
	if !profileNamePattern.MatchString(profile.Name) {
		return Profile{}, errors.New("Azure OpenAI profile name must use 1-64 lowercase letters, digits, dots, dashes, or underscores")
	}

	endpoint, err := normalizeEndpoint(profile.Endpoint)
	if err != nil {
		return Profile{}, err
	}
	profile.Endpoint = endpoint.String()
	profile.Deployment = strings.TrimSpace(profile.Deployment)
	if profile.Deployment == "" || len(profile.Deployment) > 256 || strings.ContainsAny(profile.Deployment, "\r\n\x00") {
		return Profile{}, errors.New("Azure OpenAI deployment name is invalid")
	}

	profile.TokenResource = strings.TrimRight(strings.TrimSpace(profile.TokenResource), "/")
	if profile.TokenResource == "" {
		profile.TokenResource = FoundryTokenResource
	}
	if err := validateTokenResource(profile.TokenResource); err != nil {
		return Profile{}, err
	}
	profile.AzureCLI = strings.TrimSpace(profile.AzureCLI)
	if profile.AzureCLI == "" {
		return Profile{}, errors.New("Azure CLI path is required")
	}
	if strings.ContainsAny(profile.AzureCLI, "\r\n\x00") {
		return Profile{}, errors.New("Azure CLI path is invalid")
	}
	if profile.AddedAt != "" {
		if _, err := time.Parse(time.RFC3339, profile.AddedAt); err != nil {
			return Profile{}, errors.New("Azure OpenAI profile addedAt is invalid")
		}
	}
	return profile, nil
}

func normalizeEndpoint(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("Azure OpenAI endpoint must be an absolute URL without credentials, query, or fragment")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopbackHost(parsed.Hostname())) {
		return nil, errors.New("Azure OpenAI endpoint must use HTTPS, except for loopback development")
	}
	if !loopbackHost(parsed.Hostname()) && !azureEndpointHost(parsed.Hostname()) {
		return nil, errors.New("Azure OpenAI endpoint must use an Azure OpenAI, Foundry, or Cognitive Services hostname")
	}
	path := strings.TrimRight(parsed.EscapedPath(), "/")
	switch path {
	case "", "/":
		parsed.Path = "/openai/v1"
		parsed.RawPath = ""
	case "/openai/v1":
		parsed.Path = "/openai/v1"
		parsed.RawPath = ""
	default:
		return nil, errors.New("Azure OpenAI endpoint path must be empty or /openai/v1")
	}
	return parsed, nil
}

func validateTokenResource(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("Azure token resource must be an HTTPS origin")
	}
	return nil
}

func loopbackHost(host string) bool {
	if strings.EqualFold(strings.TrimSpace(host), "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func azureEndpointHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	for _, suffix := range []string{
		".openai.azure.com",
		".openai.azure.us",
		".openai.azure.cn",
		".services.ai.azure.com",
		".services.ai.azure.us",
		".services.ai.azure.cn",
		".cognitiveservices.azure.com",
		".cognitiveservices.azure.us",
		".cognitiveservices.azure.cn",
	} {
		if strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
			return true
		}
	}
	return false
}

func (s Store) List() ([]Profile, error) {
	body, err := os.ReadFile(s.ProfilesPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var file profilesFile
	if err := json.Unmarshal(body, &file); err != nil {
		return nil, fmt.Errorf("parse Azure OpenAI profiles: %w", err)
	}
	profiles := make([]Profile, 0, len(file.Profiles))
	seen := map[string]bool{}
	for _, raw := range file.Profiles {
		profile, err := NormalizeProfile(raw)
		if err != nil {
			return nil, fmt.Errorf("Azure OpenAI profile %q: %w", raw.Name, err)
		}
		if seen[profile.Name] {
			return nil, fmt.Errorf("duplicate Azure OpenAI profile %q", profile.Name)
		}
		seen[profile.Name] = true
		profiles = append(profiles, profile)
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Name < profiles[j].Name })
	return profiles, nil
}

func (s Store) Find(name string) (Profile, bool, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	profiles, err := s.List()
	if err != nil {
		return Profile{}, false, err
	}
	for _, profile := range profiles {
		if profile.Name == name {
			return profile, true, nil
		}
	}
	return Profile{}, false, nil
}

func (s Store) Save(profile Profile) (bool, error) {
	profile, err := NormalizeProfile(profile)
	if err != nil {
		return false, err
	}
	profiles, err := s.List()
	if err != nil {
		return false, err
	}
	existed := false
	for i := range profiles {
		if profiles[i].Name != profile.Name {
			continue
		}
		existed = true
		if profile.AddedAt == "" {
			profile.AddedAt = profiles[i].AddedAt
		}
		profiles[i] = profile
		break
	}
	if profile.AddedAt == "" {
		profile.AddedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if !existed {
		profiles = append(profiles, profile)
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Name < profiles[j].Name })
	return existed, s.write(profiles)
}

func (s Store) Remove(name string) (bool, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	profiles, err := s.List()
	if err != nil {
		return false, err
	}
	kept := profiles[:0]
	removed := false
	for _, profile := range profiles {
		if profile.Name == name {
			removed = true
			continue
		}
		kept = append(kept, profile)
	}
	if !removed {
		return false, nil
	}
	return true, s.write(kept)
}

func (s Store) write(profiles []Profile) error {
	body, err := json.MarshalIndent(profilesFile{Version: 1, Profiles: profiles}, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	path := s.ProfilesPath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	if dirFile, err := os.Open(dir); err == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}
