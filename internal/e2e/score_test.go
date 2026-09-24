package e2e_test

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/dsb-labs/takt/pkg/client"
	"github.com/dsb-labs/takt/pkg/manifest"
	"github.com/dsb-labs/takt/pkg/score"
)

// The score the journey applies: a volume, a workload the other reaches by
// reference, a service, a variable the score owns, one it requires, and a
// secret it declares. The worker is behind a value, so a later render drops it
// and prune has something to remove.
//
// The score file itself is not templated, so the install name is formatted
// into the variable and secret entries. The manifests read it as .Score.Name.
const (
	scoreFile = `version: v1
name: journey
release: 1.0.0
volumes:
  - data.yaml
workloads:
  - db.yaml
  - web.yaml
  - worker.yaml
services:
  - web.service.yaml
variables:
  - name: %[1]s-config
    value: "level={{ .Values.level }}"
  - name: %[1]s-host
secrets:
  - name: %[1]s-password
`
	scoreValues = `level: info
worker: true
`
	scoreVolume = `version: v1
name: {{ .Score.Name }}-data
`
	scoreDB = `version: v1
name: {{ .Score.Name }}-db
labels:
  app: {{ .Score.Name }}
ports:
  - name: http
    to: 80
container:
  image: ` + testImage + `
`
	scoreWeb = `version: v1
name: {{ .Score.Name }}-web
labels:
  app: {{ .Score.Name }}
  tier: web
ports:
  - name: http
    to: 80
env:
  DB: ${workload:{{ .Score.Name }}-db:http}
  HOST: ${var:{{ .Score.Name }}-host}
  PASSWORD: ${secret:{{ .Score.Name }}-password}
volumes:
  - name: {{ .Score.Name }}-data
    to: /data
  - var: {{ .Score.Name }}-config
    to: /etc/app/config
container:
  image: ` + testImage + `
`
	scoreWorker = `{{- if .Values.worker }}
version: v1
name: {{ .Score.Name }}-worker
labels:
  app: {{ .Score.Name }}
container:
  image: ` + testImage + `
{{- end }}
`
	scoreService = `version: v1
name: {{ .Score.Name }}
target:
  labels:
    app: {{ .Score.Name }}
    tier: web
  port: 80
`
)

// TestScoreJourney covers a score's whole life: applied in dependency order
// with its requirements checked first, pruned when a manifest leaves it, listed
// by its label, and deleted in reverse order with its data.
func (s *Suite) TestScoreJourney() {
	name := s.workloadName()
	for _, suffix := range []string{"-db", "-web", "-worker"} {
		s.T().Cleanup(func() { s.cleanup(name + suffix) })
	}
	s.T().Cleanup(func() { s.cleanupVariable(name + "-host") })
	s.T().Cleanup(func() { s.cleanupVariable(name + "-config") })
	s.T().Cleanup(func() { s.cleanupSecret(name + "-password") })
	s.T().Cleanup(func() { s.cleanupVolume(name + "-data") })

	directory := s.T().TempDir()
	for file, content := range map[string]string{
		"score.yaml":       fmt.Sprintf(scoreFile, name),
		"values.yaml":      scoreValues,
		"data.yaml":        scoreVolume,
		"db.yaml":          scoreDB,
		"web.yaml":         scoreWeb,
		"worker.yaml":      scoreWorker,
		"web.service.yaml": scoreService,
	} {
		s.Require().NoError(os.WriteFile(filepath.Join(directory, file), []byte(content), 0o600))
	}

	rendered, err := score.Build(directory, nil, score.WithName(name))
	s.Require().NoError(err)

	// Nothing the score requires exists yet, and the check says so all at once.
	preconditions, err := score.Check(s.ctx(), s.client, rendered)
	s.Require().NoError(err)
	s.ElementsMatch([]score.Node{
		{Kind: score.KindVariable, Name: name + "-host"},
		{Kind: score.KindSecret, Name: name + "-password"},
	}, preconditions.Missing)
	s.Empty(preconditions.Unowned)

	_, err = score.Apply(s.ctx(), s.client, rendered)
	s.ErrorIs(err, score.ErrMissing)

	_, _, err = s.client.SetVariable(s.ctx(), manifest.Variable{Name: name + "-host", Value: "db.internal"})
	s.Require().NoError(err)

	_, _, err = s.client.SetSecret(s.ctx(), manifest.Secret{Name: name + "-password", Value: []byte("hunter2")})
	s.Require().NoError(err)

	report, err := score.Apply(s.ctx(), s.client, rendered)
	s.Require().NoError(err)
	s.Equal([]score.Node{
		{Kind: score.KindVolume, Name: name + "-data"},
		{Kind: score.KindVariable, Name: name + "-config"},
		{Kind: score.KindWorkload, Name: name + "-db"},
		{Kind: score.KindWorkload, Name: name + "-web"},
		{Kind: score.KindWorkload, Name: name + "-worker"},
		{Kind: score.KindService, Name: name},
	}, report.Applied)

	web := s.awaitState(name+"-web", client.WorkloadStateRunning)
	s.Equal(name, web.Spec.Labels[score.LabelName])
	s.Equal("1.0.0", web.Spec.Labels[score.LabelRelease])

	variable, err := s.client.GetVariable(s.ctx(), name+"-config")
	s.Require().NoError(err)
	s.Equal("level=info", variable.Value)
	s.Equal(name, variable.Labels[score.LabelName])

	// The same apply again changes nothing and refuses nothing: everything it
	// finds carries its own label.
	_, err = score.Apply(s.ctx(), s.client, rendered)
	s.Require().NoError(err)

	// A second install of the same score on the same host is refused where
	// the names collide, which is what the label check is for.
	_, _, err = s.client.SetVariable(s.ctx(), manifest.Variable{Name: name + "-config", Value: "level=info"})
	s.Require().NoError(err)

	preconditions, err = score.Check(s.ctx(), s.client, rendered)
	s.Require().NoError(err)
	s.Equal([]score.Node{{Kind: score.KindVariable, Name: name + "-config"}}, preconditions.Unowned)

	_, err = score.Apply(s.ctx(), s.client, rendered)
	s.ErrorIs(err, score.ErrUnowned)

	_, err = score.Apply(s.ctx(), s.client, rendered, score.WithAdopt())
	s.Require().NoError(err)

	// Dropping the worker from the values leaves it on the server carrying the
	// label and absent from the render, which is exactly what prune removes.
	overrides := filepath.Join(s.T().TempDir(), "values.yaml")
	s.Require().NoError(os.WriteFile(overrides, []byte("worker: false\n"), 0o600))

	rendered, err = score.Build(directory, []string{overrides}, score.WithName(name))
	s.Require().NoError(err)
	s.Len(rendered.Workloads, 2)

	_, err = score.Apply(s.ctx(), s.client, rendered)
	s.Require().NoError(err)

	install, err := score.Installed(s.ctx(), s.client, name)
	s.Require().NoError(err)
	s.Equal([]string{name + "-db", name + "-web", name + "-worker"}, install.Workloads)

	prunable := score.Prunable(rendered, install)
	s.Equal([]score.Node{{Kind: score.KindWorkload, Name: name + "-worker"}}, prunable)

	pruned, err := score.Destroy(s.ctx(), s.client, prunable)
	s.Require().NoError(err)
	s.Equal(prunable, pruned)

	_, err = s.client.Get(s.ctx(), name+"-worker")
	s.ErrorIs(err, client.ErrWorkloadNotFound)

	installs, err := score.List(s.ctx(), s.client)
	s.Require().NoError(err)
	s.Require().Len(installs, 1)
	s.Equal(score.Install{
		Name:      name,
		Releases:  []string{"1.0.0"},
		Volumes:   []string{name + "-data"},
		Variables: []string{name + "-config"},
		Workloads: []string{name + "-db", name + "-web"},
		Services:  []string{name},
	}, installs[0])

	// Deleting walks back through the order: the service, then web before the
	// db it references, then the variable and the volume web held, each of
	// which the server would have refused while web was still up.
	deleted, err := score.Destroy(s.ctx(), s.client, installs[0].Nodes())
	s.Require().NoError(err)
	s.Equal([]score.Node{
		{Kind: score.KindService, Name: name},
		{Kind: score.KindWorkload, Name: name + "-web"},
		{Kind: score.KindWorkload, Name: name + "-db"},
		{Kind: score.KindVariable, Name: name + "-config"},
		{Kind: score.KindVolume, Name: name + "-data"},
	}, deleted)

	installs, err = score.List(s.ctx(), s.client)
	s.Require().NoError(err)
	s.Empty(installs)

	// What the score required is not the score's to remove.
	_, err = s.client.GetVariable(s.ctx(), name+"-host")
	s.NoError(err)
}
