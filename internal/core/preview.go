package core

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ValidatePreviewServices checks saved preview configuration without accessing a host.
func ValidatePreviewServices(services []PreviewService) error {
	names := map[string]bool{}
	for _, service := range services {
		name := strings.TrimSpace(service.Name)
		if name == "" || names[name] || strings.ContainsAny(name, "\r\n,") {
			return fmt.Errorf("preview services need unique, nonempty names without commas or newlines")
		}
		names[name] = true
		if strings.TrimSpace(service.Command) == "" {
			return fmt.Errorf("preview service %s needs a command", name)
		}
		dir := filepath.Clean(service.Dir)
		if filepath.IsAbs(dir) || dir == ".." || strings.HasPrefix(dir, ".."+string(filepath.Separator)) {
			return fmt.Errorf("preview service %s directory must be inside the ticket worktree", name)
		}
	}
	return nil
}
