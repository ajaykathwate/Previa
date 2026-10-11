package starterkits

import (
	"strings"

	kitpages "github.com/ajaykathwatee/previa/web/pages/starterkits"
)

// view turns a kit into what the page shows.
func view(kit Kit, offer Offer) kitpages.Page {
	s, c := kit.Stack, kit.Choices
	p := kitpages.Page{Notes: kit.Notes, Offer: offerText(offer)}
	p.Status = "Files updated: " + strings.Join(summary(kit), ", ") + "."
	for _, st := range catalog {
		p.Stacks = append(p.Stacks, kitpages.StackOption{ID: st.ID, Name: st.Name, Kind: st.Kind, Language: st.Language, Selected: st.ID == s.ID})
	}
	p.Form = kitpages.Form{
		Stack: s.ID, StackName: s.Name, Static: s.Static, OwnFile: s.OwnFile,
		Postgres: kitpages.Service{On: c.Postgres != "", Version: c.Postgres, Versions: offer.Postgres},
		Redis:    kitpages.Service{On: c.Redis != "", Version: c.Redis, Versions: offer.Redis},
		Setup:    command(c.Setup, c.SetupWith, c.SetupCommand, s.Setup, "npm run migrate && npm run seed"),
		Worker:   command(c.Worker, c.WorkerWith, c.WorkerCommand, s.Workers, "npm run worker"),
		Docs:     c.Docs, DocsPath: cmpOr(c.DocsPath, cmpOr(s.Docs, "/docs")),
		Port: c.Port, Health: c.Health, Tier: c.Tier, Tiers: Tiers,
	}
	if d := s.Detail; d != nil {
		p.Form.Detail = &kitpages.Detail{Label: d.Label, Hint: d.Hint, Value: c.Detail, Options: d.Options}
	}
	if kit.Dockerfile != "" {
		p.Files = append(p.Files, kitpages.File{ID: "dockerfile", Name: "Dockerfile", Kind: "dockerfile", Content: kit.Dockerfile,
			Purpose: "How Previa builds your app"})
	}
	p.Files = append(p.Files, kitpages.File{ID: "previewrc", Name: ".previewrc.yaml", Kind: "yaml", Content: kit.Config,
		Purpose: "How Previa runs each preview"})
	for _, problem := range kit.Problems {
		p.Problems = append(p.Problems, kitpages.Problem{Field: problem.Field, Label: fieldLabel(kit.Stack, problem.Field), Message: problem.Message})
	}
	return p
}

// fieldLabel names a form field in the list of problems, where the message
// is read away from the field itself.
func fieldLabel(s Stack, field string) string {
	switch field {
	case "port":
		return "Port"
	case "health":
		return "Health check path"
	case "docs":
		return "Docs path"
	case "setup":
		return "Migrations"
	case "worker":
		return "Worker"
	case "postgres":
		return "PostgreSQL"
	case "redis":
		return "Redis"
	case "detail":
		if s.Detail != nil {
			return s.Detail.Label
		}
	}
	return ""
}

func command(on bool, with, text string, presets []Preset, placeholder string) kitpages.Command {
	cmd := kitpages.Command{Available: true, On: on, With: with, Text: text, Placeholder: placeholder}
	for _, preset := range presets {
		cmd.Options = append(cmd.Options, kitpages.Option{Value: preset.Key, Label: preset.Label})
	}
	return cmd
}

// summary is the kit at a glance.
func summary(kit Kit) []string {
	s, c := kit.Stack, kit.Choices
	out := []string{s.Name}
	if c.Postgres != "" {
		out = append(out, "PostgreSQL "+c.Postgres)
	}
	if c.Redis != "" {
		out = append(out, "Redis "+c.Redis)
	}
	if c.Setup {
		out = append(out, "migrations")
	}
	if c.Worker {
		out = append(out, "worker")
	}
	if c.Docs {
		out = append(out, "API docs")
	}
	return append(out, c.Tier+" size")
}

func offerText(o Offer) string {
	var parts []string
	if len(o.Postgres) > 0 {
		parts = append(parts, "PostgreSQL "+strings.Join(o.Postgres, " or "))
	}
	if len(o.Redis) > 0 {
		parts = append(parts, "Redis "+strings.Join(o.Redis, " or "))
	}
	switch {
	case o.Known && len(parts) == 0:
		return "This server offers no managed services right now."
	case o.Known:
		return "This server offers " + strings.Join(parts, " and ") + "."
	case len(parts) == 0:
		return "Managed services are checked when a worker that builds is running."
	}
	return "Previa offers " + strings.Join(parts, " and ") + ", checked again when your preview builds."
}
