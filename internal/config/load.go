package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// FileName is the config file's name inside the Gravy home directory.
const FileName = "config.yaml"

// EnvHome overrides the Gravy home directory. Used by tests and by anyone running more than one
// installation on a machine.
const EnvHome = "GRAVY_HOME"

// Home returns the Gravy home directory: $GRAVY_HOME, else ~/.gravy.
func Home() (string, error) {
	if d := os.Getenv(EnvHome); d != "" {
		return d, nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(h, ".gravy"), nil
}

// Path returns the full path of the config file within dir.
func Path(dir string) string { return filepath.Join(dir, FileName) }

// Load reads the configuration from dir, creating dir and a documented default file if none
// exists. The returned bool reports whether the file was created by this call, which onboarding
// uses to decide whether this is a first run.
func Load(dir string) (Config, bool, error) {
	path := Path(dir)

	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		cfg := Default()
		if err := writeFile(path, []byte(defaultFile)); err != nil {
			return Config{}, false, err
		}
		return cfg, true, nil
	case err != nil:
		return Config{}, false, fmt.Errorf("read %s: %w", path, err)
	}

	cfg, err := Parse(b, path)
	if err != nil {
		return Config{}, false, err
	}
	return cfg, false, nil
}

// LoadDefault loads from the default home directory.
func LoadDefault() (Config, bool, error) {
	dir, err := Home()
	if err != nil {
		return Config{}, false, err
	}
	return Load(dir)
}

// Parse decodes and validates a configuration document. filename is used only in error
// messages. Absent settings take their default, so a partial file is valid and a new setting
// does not break an existing installation.
func Parse(b []byte, filename string) (Config, error) {
	cfg := Default()

	// Strict decoding: an unknown key is a typo, and silently ignoring it would leave the user
	// with a setting they believe is in effect and is not.
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("%s: %w", filename, err)
	}

	// A second pass retains position information so semantic errors can name a line too.
	var doc yaml.Node
	loc := &locator{file: filename}
	if err := yaml.Unmarshal(b, &doc); err == nil {
		loc.doc = &doc
	}

	if err := cfg.validate(loc); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Save writes the configuration to dir, creating the directory if needed. It is used by
// onboarding write-back, so it must produce a file that Load reads back identically.
func Save(dir string, c Config) error {
	if err := c.Validate(); err != nil {
		return fmt.Errorf("refusing to save invalid configuration: %w", err)
	}
	var b bytes.Buffer
	b.WriteString(savedHeader)
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(c); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	return writeFile(Path(dir), b.Bytes())
}

// writeFile creates the parent directory and writes atomically, so an interrupted save cannot
// leave a half-written config that fails to parse on next start.
func writeFile(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.yaml")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

const savedHeader = `# ~/.gravy/config.yaml — written by Gravy.
# Per-project settings live in the database and are edited from the TUI, not here.
# See docs/ARCHITECTURE.md for what each setting does.

`
