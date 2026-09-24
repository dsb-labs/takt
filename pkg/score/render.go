package score

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	"go.yaml.in/yaml/v3"

	"github.com/dsb-labs/takt/pkg/manifest"
)

type (
	// The Rendered type is what rendering a score produces: every manifest it
	// names, rendered against the values and parsed, together with the variables
	// and secrets it declares.
	//
	// It is what apply works from, and what show reports. Every manifest in it has
	// passed the same validation the single-resource commands run, and carries
	// the two score labels.
	Rendered struct {
		// The name the score is installed as, which every resource is labelled
		// with. The score's own name unless WithName chose another.
		Name string
		// The release of the score, which every resource is labelled with.
		Release string
		// The name of the package the score describes.
		Package string
		// The rendered volume manifests, in the order the score lists them.
		Volumes []manifest.Volume
		// The rendered workload manifests, in the order the score lists them.
		Workloads []manifest.Spec
		// The rendered service manifests, in the order the score lists them.
		Services []manifest.Service
		// The variables the score owns, with their values rendered and the
		// score labels attached.
		Variables []manifest.Variable
		// The names of the variables the score requires but does not set.
		Required []string
		// The names of the secrets the score declares.
		Secrets []string
		// Every document rendered, in the order it was produced, including any
		// that rendered to nothing. On a failed render, the last document is the
		// one that failed, so a caller can print it beside the error.
		Documents []Document
	}

	// The Document type is one rendered YAML document and the file it came from.
	Document struct {
		// The path of the file that produced the document, relative to the score.
		Source string
		// The rendered text of the document.
		Text string
		// Whether the document rendered to nothing but whitespace and comments,
		// and so produced no resource.
		Skipped bool
	}

	// The Option type modifies how a score is rendered.
	Option func(*config)

	config struct {
		name string
	}

	// The data every template is executed against.
	data struct {
		Values Values
		Score  scoreData
	}

	// What templates read as .Score.
	scoreData struct {
		Name    string
		Release string
		Package string
	}
)

var (
	// ErrDuplicateName is returned when two manifests in a score name the same
	// resource of one kind.
	ErrDuplicateName = errors.New("duplicate name")
	// ErrReservedLabel is returned when a manifest sets a label the score owns.
	ErrReservedLabel = errors.New("reserved label")
	// ErrInvalidDocument is returned when a rendered document does not parse as
	// the manifest kind the score listed it under. The document is the last in
	// the returned Rendered, so a caller can print it beside the error.
	ErrInvalidDocument = errors.New("invalid document")
)

// The Sprig helpers that survive its hermetic filter but still depend on
// something other than their inputs: the clock, a random source or key
// generation. Render has to be a pure function of the directory and the values,
// or it stops working as a review artefact and prune's comparison becomes
// meaningless.
var impureFunctions = []string{
	"ago",
	"bcrypt",
	"derivePassword",
	"genCA",
	"genCAWithKey",
	"genPrivateKey",
	"genSelfSignedCert",
	"genSelfSignedCertWithKey",
	"genSignedCert",
	"genSignedCertWithKey",
	"htpasswd",
	"randInt",
	"shuffle",
}

// WithName modifies the name the score is installed as, which templates read
// as .Score.Name and every resource is labelled with. It defaults to the
// score's own name.
func WithName(name string) Option {
	return func(c *config) {
		c.name = name
	}
}

// Render renders every manifest the score names against the values and parses
// the results, returning them together with the variables and secrets the score
// declares.
//
// A document that renders to nothing but whitespace and comments is skipped. A
// file that renders to a multi-document stream yields several resources. Two
// resources of one kind with the same name fail the render, as does a manifest
// that sets either score label itself. On failure the returned Rendered still
// holds every document produced so far, the failing one last.
func Render(score Score, values Values, options ...Option) (Rendered, error) {
	cfg := &config{name: score.Name}
	for _, option := range options {
		option(cfg)
	}

	if err := manifest.ValidateName(cfg.name); err != nil {
		return Rendered{}, fmt.Errorf("invalid install name: %w", err)
	}

	renderer := renderer{
		score:  score,
		labels: map[string]string{LabelName: cfg.name, LabelRelease: score.Release},
		data: data{
			Values: values,
			Score:  scoreData{Name: cfg.name, Release: score.Release, Package: score.Name},
		},
		funcs: funcs(),
	}

	rendered := Rendered{
		Name:    cfg.name,
		Release: score.Release,
		Package: score.Name,
		Secrets: make([]string, 0, len(score.Secrets)),
	}

	for _, secret := range score.Secrets {
		rendered.Secrets = append(rendered.Secrets, secret.Name)
	}

	if err := renderer.volumes(&rendered); err != nil {
		return rendered, err
	}

	if err := renderer.workloads(&rendered); err != nil {
		return rendered, err
	}

	if err := renderer.services(&rendered); err != nil {
		return rendered, err
	}

	if err := renderer.variables(&rendered); err != nil {
		return rendered, err
	}

	return rendered, nil
}

// Labels returns the labels every resource of the rendered score carries.
func (r Rendered) Labels() map[string]string {
	return map[string]string{LabelName: r.Name, LabelRelease: r.Release}
}

// The renderer type holds what rendering every file of one score shares.
type renderer struct {
	score  Score
	labels map[string]string
	data   data
	funcs  template.FuncMap
}

func (r renderer) volumes(rendered *Rendered) error {
	return each(r, rendered, r.score.Volumes, manifest.ParseVolume, func(volume *manifest.Volume) (string, *map[string]string) {
		return volume.Name, &volume.Labels
	}, func(volume manifest.Volume) {
		rendered.Volumes = append(rendered.Volumes, volume)
	})
}

func (r renderer) workloads(rendered *Rendered) error {
	return each(r, rendered, r.score.Workloads, manifest.ParseWorkload, func(spec *manifest.Spec) (string, *map[string]string) {
		return spec.Name, &spec.Labels
	}, func(spec manifest.Spec) {
		rendered.Workloads = append(rendered.Workloads, spec)
	})
}

func (r renderer) services(rendered *Rendered) error {
	return each(r, rendered, r.score.Services, manifest.ParseService, func(service *manifest.Service) (string, *map[string]string) {
		return service.Name, &service.Labels
	}, func(service manifest.Service) {
		rendered.Services = append(rendered.Services, service)
	})
}

// each renders every file in paths, parses each document with parse, labels it
// and hands it to collect.
//
// The identity function returns the resource's name and a pointer to its labels,
// which is how one loop stamps three manifest kinds without each having to
// implement an interface for the purpose. It takes a pointer so that stamping a
// manifest with no labels of its own reaches the value collect receives.
func each[T any](r renderer, rendered *Rendered, paths []string, parse func(io.Reader) (T, error), identity func(*T) (string, *map[string]string), collect func(T)) error {
	names := make(map[string]string, len(paths))

	for _, path := range paths {
		documents, err := r.render(path)
		if err != nil {
			return err
		}

		for _, document := range documents {
			empty, err := isEmpty(document.Text)
			if err != nil {
				rendered.Documents = append(rendered.Documents, document)
				return fmt.Errorf("%w: failed to parse %s: %w", ErrInvalidDocument, path, err)
			}

			document.Skipped = empty
			rendered.Documents = append(rendered.Documents, document)

			if empty {
				continue
			}

			resource, err := parse(strings.NewReader(document.Text))
			if err != nil {
				return fmt.Errorf("%w: %s: %w", ErrInvalidDocument, path, err)
			}

			name, labels := identity(&resource)
			if previous, ok := names[name]; ok {
				return fmt.Errorf("%w: %s and %s both name %s", ErrDuplicateName, previous, path, name)
			}

			names[name] = path

			if err = r.label(labels); err != nil {
				return fmt.Errorf("failed to label %s: %w", path, err)
			}

			collect(resource)
		}
	}

	return nil
}

// variables renders the value of every owned variable and records the name of
// every required one.
func (r renderer) variables(rendered *Rendered) error {
	for _, variable := range r.score.Variables {
		if !variable.Owned() {
			rendered.Required = append(rendered.Required, variable.Name)
			continue
		}

		value, err := r.value(variable)
		if err != nil {
			return err
		}

		rendered.Variables = append(rendered.Variables, manifest.Variable{
			Name:   variable.Name,
			Value:  value,
			Labels: maps.Clone(r.labels),
		})
	}

	return nil
}

// value renders an owned variable's value, from its file or its inline value.
func (r renderer) value(variable Variable) (string, error) {
	if variable.File == "" {
		value, err := r.execute("variable "+variable.Name, variable.Value)
		if err != nil {
			return "", fmt.Errorf("failed to render variable %s: %w", variable.Name, err)
		}

		return value, nil
	}

	text, err := r.read(variable.File)
	if err != nil {
		return "", err
	}

	value, err := r.execute(variable.File, text)
	if err != nil {
		return "", fmt.Errorf("failed to render %s: %w", variable.File, err)
	}

	return value, nil
}

// render renders one manifest file and splits the result into its documents.
func (r renderer) render(path string) ([]Document, error) {
	text, err := r.read(path)
	if err != nil {
		return nil, err
	}

	output, err := r.execute(path, text)
	if err != nil {
		return nil, fmt.Errorf("failed to render %s: %w", path, err)
	}

	documents := make([]Document, 0, 1)
	for _, text := range split(output) {
		documents = append(documents, Document{Source: path, Text: text})
	}

	return documents, nil
}

// read returns the contents of a file named by the score, resolved against the
// score's directory.
func (r renderer) read(path string) (string, error) {
	data, err := os.ReadFile(filepath.Join(r.score.Directory, path))
	if err != nil {
		return "", fmt.Errorf("failed to read %s: %w", path, err)
	}

	return string(data), nil
}

// execute parses text as a template named name and executes it against the
// score's data.
//
// A value the template names that the values do not hold is an error rather
// than "<no value>", because a manifest with a hole in it is more likely a typo
// in the values file than an intent.
func (r renderer) execute(name, text string) (string, error) {
	tmpl, err := template.New(name).Option("missingkey=error").Funcs(r.funcs).Parse(text)
	if err != nil {
		return "", err
	}

	var out bytes.Buffer
	if err = tmpl.Execute(&out, r.data); err != nil {
		return "", err
	}

	return out.String(), nil
}

// label stamps the score labels onto a manifest's labels, refusing a manifest
// that set either itself.
func (r renderer) label(labels *map[string]string) error {
	for key := range r.labels {
		if _, ok := (*labels)[key]; ok {
			return fmt.Errorf("%w: %s is set by the score", ErrReservedLabel, key)
		}
	}

	if *labels == nil {
		*labels = make(map[string]string, len(r.labels))
	}

	maps.Copy(*labels, r.labels)

	return nil
}

// funcs returns the template functions manifests may call: Sprig's hermetic
// set with the remaining impure helpers removed.
func funcs() template.FuncMap {
	out := sprig.HermeticTxtFuncMap()
	for _, name := range impureFunctions {
		delete(out, name)
	}

	return out
}

// split divides a rendered file into its YAML documents, on the document
// separator alone on a line. A file with no separator is one document.
func split(text string) []string {
	var documents []string

	var current strings.Builder
	for line := range strings.SplitSeq(text, "\n") {
		if strings.TrimRight(line, " \t\r") == "---" {
			documents = append(documents, current.String())
			current.Reset()
			continue
		}

		current.WriteString(line)
		current.WriteString("\n")
	}

	return append(documents, current.String())
}

// isEmpty reports whether a document holds no content: nothing, or only
// whitespace and comments.
func isEmpty(text string) (bool, error) {
	var node yaml.Node
	err := yaml.NewDecoder(strings.NewReader(text)).Decode(&node)
	switch {
	case errors.Is(err, io.EOF):
		return true, nil
	case err != nil:
		return false, err
	default:
		return node.Kind == 0, nil
	}
}

// Build loads the score at location, merges its values with the given values
// files and renders it: the three steps every command takes, in one call.
//
// On a render failure the returned Rendered is what Render returned, so the
// failing document is still there to print.
func Build(location string, values []string, options ...Option) (Rendered, error) {
	loaded, err := Load(location)
	if err != nil {
		return Rendered{}, err
	}

	merged, err := loaded.Values(values...)
	if err != nil {
		return Rendered{}, err
	}

	return Render(loaded, merged, options...)
}
