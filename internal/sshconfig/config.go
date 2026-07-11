package sshconfig

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

const DefaultPort = 4433

type ParsedTarget struct {
	Original        string
	Host            string
	Username        string
	HasExplicitUser bool
}

type ResolvedTarget struct {
	OriginalHost   string
	Hostname       string
	Username       string
	Port           int
	IdentityFiles  []string
	IdentitiesOnly bool
}

type ResolveOptions struct {
	ExplicitIdentityFile string
	ExplicitPort         int
	ExplicitPortSet      bool
	ConfigPath           string
	HomeDir              string
	LocalUsername        string
}

type block struct {
	patterns []string
	lines    []directive
}

type directive struct {
	key   string
	value string
	line  int
}

type appliedConfig struct {
	hostname       string
	user           string
	port           int
	hasPort        bool
	identityFiles  []string
	identitiesOnly bool
	hasIDsOnly     bool
}

func ParseTarget(raw string) (ParsedTarget, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ParsedTarget{}, errors.New("target is empty")
	}
	if strings.HasPrefix(raw, "@") || strings.HasSuffix(raw, "@") {
		return ParsedTarget{}, fmt.Errorf("invalid target %q", raw)
	}
	if userPart, hostPart, ok := strings.Cut(raw, "@"); ok {
		if userPart == "" || hostPart == "" {
			return ParsedTarget{}, fmt.Errorf("invalid target %q", raw)
		}
		return ParsedTarget{
			Original:        raw,
			Host:            hostPart,
			Username:        userPart,
			HasExplicitUser: true,
		}, nil
	}
	return ParsedTarget{Original: raw, Host: raw}, nil
}

func Resolve(target ParsedTarget, opts ResolveOptions) (ResolvedTarget, error) {
	homeDir, err := resolveHomeDir(opts.HomeDir)
	if err != nil {
		return ResolvedTarget{}, err
	}
	localUser, err := resolveLocalUsername(opts.LocalUsername)
	if err != nil {
		return ResolvedTarget{}, err
	}

	configPath := opts.ConfigPath
	if configPath == "" {
		configPath = filepath.Join(homeDir, ".ssh", "config")
	}

	blocks, err := parseConfig(configPath)
	if err != nil {
		return ResolvedTarget{}, err
	}

	applied, err := applyBlocks(blocks, target.Host, target.Username, target.HasExplicitUser)
	if err != nil {
		return ResolvedTarget{}, err
	}

	username := target.Username
	if username == "" {
		if applied.user != "" {
			username = applied.user
		} else {
			username = localUser
		}
	}

	hostname := target.Host
	if applied.hostname != "" {
		hostname = expandValue(applied.hostname, homeDir, target.Host, hostname, username, localUser)
	}

	port := DefaultPort
	if opts.ExplicitPortSet {
		port = opts.ExplicitPort
	} else if applied.hasPort {
		port = applied.port
	}

	var identities []string
	switch {
	case opts.ExplicitIdentityFile != "":
		identities = []string{expandValue(opts.ExplicitIdentityFile, homeDir, target.Host, hostname, username, localUser)}
	case len(applied.identityFiles) > 0:
		identities = make([]string, 0, len(applied.identityFiles))
		for _, ident := range applied.identityFiles {
			identities = append(identities, expandValue(ident, homeDir, target.Host, hostname, username, localUser))
		}
	default:
		identities = []string{filepath.Join(homeDir, ".ssh", "id_ed25519")}
	}

	return ResolvedTarget{
		OriginalHost:   target.Host,
		Hostname:       hostname,
		Username:       username,
		Port:           port,
		IdentityFiles:  identities,
		IdentitiesOnly: applied.identitiesOnly,
	}, nil
}

func parseConfig(configPath string) ([]block, error) {
	file, err := os.Open(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("SSH config could not be parsed: open %s: %w", configPath, err)
	}
	defer file.Close()

	blocks := []block{{}}
	scanner := bufio.NewScanner(file)
	for lineNum := 1; scanner.Scan(); lineNum++ {
		line := strings.TrimSpace(stripComment(scanner.Text()))
		if line == "" {
			continue
		}

		parts := strings.Fields(line)
		if len(parts) < 2 {
			return nil, fmt.Errorf("SSH config could not be parsed: %s:%d missing value", configPath, lineNum)
		}

		key := strings.ToLower(parts[0])
		value := strings.TrimSpace(line[len(parts[0]):])
		value = strings.TrimSpace(value)

		if key == "host" {
			patterns := strings.Fields(value)
			if len(patterns) == 0 {
				return nil, fmt.Errorf("SSH config could not be parsed: %s:%d empty Host directive", configPath, lineNum)
			}
			blocks = append(blocks, block{patterns: patterns})
			continue
		}

		blocks[len(blocks)-1].lines = append(blocks[len(blocks)-1].lines, directive{
			key:   key,
			value: value,
			line:  lineNum,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("SSH config could not be parsed: read %s: %w", configPath, err)
	}
	return blocks, nil
}

func applyBlocks(blocks []block, originalHost, explicitUser string, hasExplicitUser bool) (appliedConfig, error) {
	var cfg appliedConfig
	for _, block := range blocks {
		match, err := blockMatches(block.patterns, originalHost)
		if err != nil {
			return cfg, err
		}
		if !match {
			continue
		}

		for _, line := range block.lines {
			switch line.key {
			case "hostname":
				if cfg.hostname == "" {
					cfg.hostname = line.value
				}
			case "user":
				if !hasExplicitUser && cfg.user == "" {
					cfg.user = line.value
				}
			case "port":
				if !cfg.hasPort {
					port, err := strconv.Atoi(line.value)
					if err != nil || port <= 0 || port > 65535 {
						return cfg, fmt.Errorf("SSH config could not be parsed: invalid Port %q on line %d", line.value, line.line)
					}
					cfg.port = port
					cfg.hasPort = true
				}
			case "identityfile":
				cfg.identityFiles = append(cfg.identityFiles, line.value)
			case "identitiesonly":
				if !cfg.hasIDsOnly {
					v, err := parseBool(line.value)
					if err != nil {
						return cfg, fmt.Errorf("SSH config could not be parsed: invalid IdentitiesOnly %q on line %d", line.value, line.line)
					}
					cfg.identitiesOnly = v
					cfg.hasIDsOnly = true
				}
			}
		}
	}

	if hasExplicitUser {
		cfg.user = explicitUser
	}
	return cfg, nil
}

func blockMatches(patterns []string, host string) (bool, error) {
	if len(patterns) == 0 {
		return true, nil
	}
	positive := false
	matched := false
	for _, pattern := range patterns {
		negated := strings.HasPrefix(pattern, "!")
		pat := strings.TrimPrefix(pattern, "!")
		ok, err := path.Match(pat, host)
		if err != nil {
			return false, fmt.Errorf("SSH config could not be parsed: invalid Host pattern %q: %w", pattern, err)
		}
		if negated && ok {
			return false, nil
		}
		if !negated {
			positive = true
			if ok {
				matched = true
			}
		}
	}
	return positive && matched, nil
}

func expandValue(value, homeDir, originalHost, resolvedHost, remoteUser, localUser string) string {
	if strings.HasPrefix(value, "~") {
		switch {
		case value == "~":
			value = homeDir
		case strings.HasPrefix(value, "~/"):
			value = filepath.Join(homeDir, strings.TrimPrefix(value, "~/"))
		}
	}
	replacer := strings.NewReplacer(
		"%h", resolvedHost,
		"%r", remoteUser,
		"%u", localUser,
	)
	if resolvedHost == "" {
		replacer = strings.NewReplacer(
			"%h", originalHost,
			"%r", remoteUser,
			"%u", localUser,
		)
	}
	return replacer.Replace(value)
}

func stripComment(line string) string {
	for i := 0; i < len(line); i++ {
		if line[i] == '#' {
			if i == 0 || line[i-1] != '\\' {
				return line[:i]
			}
		}
	}
	return line
}

func parseBool(raw string) (bool, error) {
	switch strings.ToLower(raw) {
	case "yes", "true", "on":
		return true, nil
	case "no", "false", "off":
		return false, nil
	default:
		return false, fmt.Errorf("invalid boolean %q", raw)
	}
}

func resolveHomeDir(home string) (string, error) {
	if home != "" {
		return home, nil
	}
	current, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("resolve current home directory: %w", err)
	}
	return current.HomeDir, nil
}

func resolveLocalUsername(name string) (string, error) {
	if name != "" {
		return name, nil
	}
	current, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("resolve current username: %w", err)
	}
	return current.Username, nil
}
