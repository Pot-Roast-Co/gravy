package tui

import (
	"fmt"
	"strings"

	"github.com/pot-roast-co/gravy/internal/core"
)

func previewFields(project core.Project) []projectField {
	fields := []projectField{{
		Label: "preview services",
		Hint:  "names separated by commas (e.g. Backend, Frontend); edit each service below; empty uses preview command",
		Get: func(p core.Project) string {
			var names []string
			for _, service := range p.PreviewServices {
				names = append(names, service.Name)
			}
			return strings.Join(names, ", ")
		},
		Set: func(p *core.Project, input string) error {
			var services []core.PreviewService
			names := map[string]bool{}
			if strings.TrimSpace(input) != "" {
				for _, name := range strings.Split(input, ",") {
					name = strings.TrimSpace(name)
					if name == "" || names[name] || strings.ContainsAny(name, "\r\n") {
						return fmt.Errorf("each service needs a unique name")
					}
					names[name] = true
					service := core.PreviewService{Name: name, Dir: "."}
					for _, existing := range p.PreviewServices {
						if existing.Name == name {
							service = existing
							break
						}
					}
					services = append(services, service)
				}
			}
			p.PreviewServices = services
			return nil
		},
	}}
	for index, service := range project.PreviewServices {
		for _, field := range []struct{ label, hint string }{
			{"directory", "relative to the ticket worktree, e.g. backend or frontend"},
			{"command", "command to start this service; it runs alongside the other services"},
			{"environment file", "optional shell environment file, relative to service directory or an absolute local path"},
		} {
			fields = append(fields, projectField{
				Label: service.Name + " " + field.label, Hint: field.hint,
				Get: func(p core.Project) string {
					s := p.PreviewServices[index]
					switch field.label {
					case "directory":
						return s.Dir
					case "command":
						return s.Command
					default:
						return s.EnvFile
					}
				},
				Set: func(p *core.Project, value string) error {
					s := &p.PreviewServices[index]
					switch field.label {
					case "directory":
						s.Dir = strings.TrimSpace(value)
					case "command":
						s.Command = strings.TrimSpace(value)
					default:
						s.EnvFile = strings.TrimSpace(value)
					}
					return nil
				},
			})
		}
	}
	return fields
}
