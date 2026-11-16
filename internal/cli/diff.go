package cli

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/veksa/clowdr/internal/api"
	"github.com/veksa/clowdr/internal/config"
	"github.com/veksa/clowdr/internal/spec"
)

// fieldChange is one differing field of a service spec, by its deploy.yml-ish name.
type fieldChange struct {
	Field string `json:"field"`
	Old   string `json:"old"`
	New   string `json:"new"`
}

// serviceDiff is what `clowctl apply` would do with one service.
type serviceDiff struct {
	Service string        `json:"service"`
	Action  string        `json:"action"` // unchanged | reload | redeploy | not-deployed | not-in-deploy.yml | analytics | analytics-unsupported
	Changes []fieldChange `json:"changes,omitempty"`
}

func (a *app) diffCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "diff [service...]",
		Short: "What apply would change: deploy.yml against the spec on the server (exit code 1 if anything differs)",
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := a.needDeploy()
			if err != nil {
				return err
			}
			t, err := a.target(true)
			if err != nil {
				return err
			}
			var remote []api.ServiceStatus
			if err := t.client.Do(cmd.Context(), "GET", api.Path("projects", t.project, "services"), nil, &remote); err != nil {
				return err
			}
			onServer := map[string]spec.Service{}
			for _, s := range remote {
				onServer[s.Name] = s.Spec
			}
			names := args
			if len(names) == 0 {
				names = d.ServiceNames()
			}
			var out []serviceDiff
			for _, n := range names {
				local, err := d.Spec(n, a.env)
				if err != nil {
					return err
				}
				local.Defaults()
				cur, ok := onServer[n]
				switch {
				case !ok:
					out = append(out, serviceDiff{Service: n, Action: "not-deployed"})
				case reflect.DeepEqual(cur, local):
					out = append(out, serviceDiff{Service: n, Action: "unchanged"})
				default:
					act := "redeploy"
					if api.IngressOnly(cur, local) {
						act = "reload"
					}
					out = append(out, serviceDiff{Service: n, Action: act, Changes: specDiff(cur, local)})
				}
			}
			if len(args) == 0 {
				var extra []string
				for n := range onServer {
					if _, ok := d.Services[n]; !ok {
						extra = append(extra, n)
					}
				}
				sort.Strings(extra)
				for _, n := range extra {
					out = append(out, serviceDiff{Service: n, Action: "not-in-deploy.yml"})
				}
			}

			if remote, same, supported, err := analyticsSync(cmd.Context(), t.client, t.project, d.Analytics); err != nil {
				return err
			} else if !same {
				act := "analytics"
				if !supported {
					act = "analytics-unsupported"
				}
				out = append(out, serviceDiff{Service: "analytics", Action: act, Changes: []fieldChange{{Field: "analytics", Old: confJSON(remote), New: confJSON(d.Analytics)}}})
			}

			changed := false
			for _, s := range out {
				if s.Action != "unchanged" {
					changed = true
				}
			}
			if a.json() {
				if err := a.printJSON(out); err != nil {
					return err
				}
			} else {
				a.printDiff(out)
			}
			if changed {
				return exitError(1)
			}
			return nil
		},
	}
}

func (a *app) printDiff(out []serviceDiff) {
	for _, s := range out {
		switch s.Action {
		case "unchanged":
			fmt.Fprintf(a.out, "  %s: unchanged\n", s.Service)
		case "not-deployed":
			fmt.Fprintf(a.out, "+ %s: not deployed yet (apply skips it, use clowctl deploy %s)\n", s.Service, s.Service)
		case "not-in-deploy.yml":
			fmt.Fprintf(a.out, "- %s: on the server but not in deploy.yml (clowctl rm %s to remove)\n", s.Service, s.Service)
		case "reload":
			fmt.Fprintf(a.out, "~ %s: routes change, apply reloads nginx\n", s.Service)
		case "analytics":
			fmt.Fprintln(a.out, "~ analytics: the config changes, apply sends it")
		case "analytics-unsupported":
			fmt.Fprintln(a.out, "! analytics: deploy.yml has it, but the server does not support it (update clowdrd)")
		default:
			fmt.Fprintf(a.out, "~ %s: apply redeploys the current release with the new spec\n", s.Service)
		}
		for _, c := range s.Changes {
			fmt.Fprintf(a.out, "    %s: %s → %s\n", c.Field, c.Old, c.New)
		}
	}
}

// specDiff lists the fields that differ between two specs, nested structs flattened
// ("health.path").
func specDiff(old, new spec.Service) []fieldChange {
	var out []fieldChange
	walkDiff("", reflect.ValueOf(old), reflect.ValueOf(new), &out)
	return out
}

var durationType = reflect.TypeOf(time.Duration(0))

func walkDiff(prefix string, a, b reflect.Value, out *[]fieldChange) {
	t := a.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" || name == "name" && prefix == "" {
			continue
		}
		name = prefix + name
		fa, fb := a.Field(i), b.Field(i)
		if reflect.DeepEqual(fa.Interface(), fb.Interface()) {
			continue
		}
		if fa.Kind() == reflect.Struct && fa.Type() != durationType {
			walkDiff(name+".", fa, fb, out)
			continue
		}
		*out = append(*out, fieldChange{Field: name, Old: formatField(name, fa), New: formatField(name, fb)})
	}
}

// formatField renders a spec value the way it is written in deploy.yml.
func formatField(name string, v reflect.Value) string {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return "-"
		}
		v = v.Elem()
	}
	if v.Type() == durationType {
		if v.Int() == 0 {
			return "-"
		}
		return time.Duration(v.Int()).String()
	}
	switch v.Kind() {
	case reflect.Int64:
		if strings.HasSuffix(name, "memory") || strings.HasSuffix(name, "max_body") {
			if v.Int() == 0 {
				return "-"
			}
			return config.FormatBytes(v.Int())
		}
	case reflect.String:
		if v.String() == "" {
			return "-"
		}
		return v.String()
	case reflect.Slice:
		if v.Len() == 0 {
			return "-"
		}
		parts := make([]string, v.Len())
		for i := range parts {
			parts[i] = formatField(name, v.Index(i))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case reflect.Map:
		if v.Len() == 0 {
			return "-"
		}
		keys := v.MapKeys()
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = fmt.Sprintf("%v=%v", k, v.MapIndex(k))
		}
		sort.Strings(parts)
		return "{" + strings.Join(parts, ", ") + "}"
	case reflect.Struct:
		var parts []string
		t := v.Type()
		for i := range t.NumField() {
			fv := v.Field(i)
			if fv.IsZero() {
				continue
			}
			fn := strings.Split(t.Field(i).Tag.Get("json"), ",")[0]
			parts = append(parts, fn+"="+formatField(fn, fv))
		}
		return "{" + strings.Join(parts, " ") + "}"
	}
	return fmt.Sprint(v.Interface())
}
