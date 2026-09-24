package score_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/pkg/client"
	"github.com/dsb-labs/takt/pkg/manifest"
	"github.com/dsb-labs/takt/pkg/score"
)

// blog renders the blog fixture, whose plan is one volume, two owned
// variables, three workloads and a service, requiring a variable and a secret.
func blog(t *testing.T) score.Rendered {
	t.Helper()

	rendered, err := score.Build("testdata/blog", nil)
	require.NoError(t, err)

	return rendered
}

// owned are the labels a resource this score applied carries.
var owned = map[string]string{score.LabelName: "blog", score.LabelRelease: "1.4.0"}

// expectNothingExists sets up a server holding none of the score's resources.
func expectNothingExists(c *MockClient) {
	c.EXPECT().GetVolume(mock.Anything, "blog-pgdata").Return(client.Volume{}, client.ErrVolumeNotFound)
	c.EXPECT().GetVariable(mock.Anything, "motd").Return(client.Variable{}, client.ErrVariableNotFound)
	c.EXPECT().GetVariable(mock.Anything, "web-config").Return(client.Variable{}, client.ErrVariableNotFound)
	c.EXPECT().Get(mock.Anything, "blog-cron").Return(client.Workload{}, client.ErrWorkloadNotFound)
	c.EXPECT().Get(mock.Anything, "blog-db").Return(client.Workload{}, client.ErrWorkloadNotFound)
	c.EXPECT().Get(mock.Anything, "blog-web").Return(client.Workload{}, client.ErrWorkloadNotFound)
	c.EXPECT().GetService(mock.Anything, "blog").Return(client.Service{}, client.ErrServiceNotFound)
}

// expectRequirementsExist sets up a server holding what the blog requires.
func expectRequirementsExist(c *MockClient) {
	c.EXPECT().GetVariable(mock.Anything, "db-host").Return(client.Variable{Name: "db-host"}, nil)
	c.EXPECT().GetSecret(mock.Anything, "db-password").Return(client.Secret{Name: "db-password"}, nil)
}

func TestCheck(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name       string
		SetupMocks func(*MockClient)
		Expected   score.Preconditions
		ExpectsErr bool
	}{
		{
			Name: "nothing stands in the way",
			SetupMocks: func(c *MockClient) {
				expectRequirementsExist(c)
				expectNothingExists(c)
			},
		},
		{
			Name: "every missing requirement is reported at once",
			SetupMocks: func(c *MockClient) {
				c.EXPECT().GetVariable(mock.Anything, "db-host").Return(client.Variable{}, client.ErrVariableNotFound)
				c.EXPECT().GetSecret(mock.Anything, "db-password").Return(client.Secret{}, client.ErrSecretNotFound)
				expectNothingExists(c)
			},
			Expected: score.Preconditions{
				Missing: []score.Node{
					{Kind: score.KindVariable, Name: "db-host"},
					{Kind: score.KindSecret, Name: "db-password"},
				},
			},
		},
		{
			Name: "a resource without the label is unowned and one with it is not",
			SetupMocks: func(c *MockClient) {
				expectRequirementsExist(c)
				c.EXPECT().GetVolume(mock.Anything, "blog-pgdata").Return(client.Volume{Name: "blog-pgdata"}, nil)
				c.EXPECT().GetVariable(mock.Anything, "motd").Return(client.Variable{Name: "motd", Labels: owned}, nil)
				c.EXPECT().GetVariable(mock.Anything, "web-config").Return(client.Variable{Name: "web-config", Labels: map[string]string{score.LabelName: "other"}}, nil)
				c.EXPECT().Get(mock.Anything, "blog-cron").Return(client.Workload{}, client.ErrWorkloadNotFound)
				c.EXPECT().Get(mock.Anything, "blog-db").Return(client.Workload{Spec: manifest.Spec{Labels: owned}}, nil)
				c.EXPECT().Get(mock.Anything, "blog-web").Return(client.Workload{Spec: manifest.Spec{Labels: map[string]string{"app": "blog"}}}, nil)
				c.EXPECT().GetService(mock.Anything, "blog").Return(client.Service{Labels: owned}, nil)
			},
			Expected: score.Preconditions{
				Unowned: []score.Node{
					{Kind: score.KindVolume, Name: "blog-pgdata"},
					{Kind: score.KindVariable, Name: "web-config"},
					{Kind: score.KindWorkload, Name: "blog-web"},
				},
			},
		},
		{
			Name: "a lookup that fails is an error",
			SetupMocks: func(c *MockClient) {
				c.EXPECT().GetVariable(mock.Anything, "db-host").Return(client.Variable{}, errors.New("boom"))
			},
			ExpectsErr: true,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			c := NewMockClient(t)
			tc.SetupMocks(c)

			actual, err := score.Check(t.Context(), c, blog(t))
			if tc.ExpectsErr {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.Expected, actual)
		})
	}
}

func TestApply(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name       string
		Options    []score.ApplyOption
		SetupMocks func(*MockClient)
		Expected   score.Report
		ExpectErr  error
		ExpectsErr bool
	}{
		{
			Name: "applies every step in order",
			SetupMocks: func(c *MockClient) {
				expectRequirementsExist(c)
				expectNothingExists(c)

				c.EXPECT().ApplyVolume(mock.Anything, mock.MatchedBy(func(volume manifest.Volume) bool {
					return volume.Name == "blog-pgdata" && volume.Labels[score.LabelName] == "blog"
				})).Return(client.Volume{}, nil)
				c.EXPECT().SetVariable(mock.Anything, mock.MatchedBy(func(variable manifest.Variable) bool {
					return variable.Name == "motd" && variable.Value == "HELLO" && variable.Labels[score.LabelRelease] == "1.4.0"
				})).Return(client.Variable{}, true, nil)
				c.EXPECT().SetVariable(mock.Anything, mock.MatchedBy(func(variable manifest.Variable) bool {
					return variable.Name == "web-config"
				})).Return(client.Variable{}, true, nil)
				c.EXPECT().Apply(mock.Anything, mock.MatchedBy(func(spec manifest.Spec) bool { return spec.Name == "blog-cron" })).Return(client.Workload{}, true, nil)
				c.EXPECT().Apply(mock.Anything, mock.MatchedBy(func(spec manifest.Spec) bool { return spec.Name == "blog-db" })).Return(client.Workload{}, true, nil)
				c.EXPECT().Apply(mock.Anything, mock.MatchedBy(func(spec manifest.Spec) bool { return spec.Name == "blog-web" })).Return(client.Workload{}, true, nil)
				c.EXPECT().ApplyService(mock.Anything, mock.MatchedBy(func(service manifest.Service) bool { return service.Name == "blog" })).Return(client.Service{}, nil)
			},
			Expected: score.Report{
				Applied: []score.Node{
					{Kind: score.KindVolume, Name: "blog-pgdata"},
					{Kind: score.KindVariable, Name: "motd"},
					{Kind: score.KindVariable, Name: "web-config"},
					{Kind: score.KindWorkload, Name: "blog-cron"},
					{Kind: score.KindWorkload, Name: "blog-db"},
					{Kind: score.KindWorkload, Name: "blog-web"},
					{Kind: score.KindService, Name: "blog"},
				},
			},
		},
		{
			Name: "applies nothing while a requirement is missing",
			SetupMocks: func(c *MockClient) {
				c.EXPECT().GetVariable(mock.Anything, "db-host").Return(client.Variable{}, client.ErrVariableNotFound)
				c.EXPECT().GetSecret(mock.Anything, "db-password").Return(client.Secret{Name: "db-password"}, nil)
				expectNothingExists(c)
			},
			ExpectErr: score.ErrMissing,
		},
		{
			Name: "applies nothing over a resource it does not own",
			SetupMocks: func(c *MockClient) {
				expectRequirementsExist(c)
				c.EXPECT().GetVolume(mock.Anything, "blog-pgdata").Return(client.Volume{Name: "blog-pgdata"}, nil)
				c.EXPECT().GetVariable(mock.Anything, "motd").Return(client.Variable{}, client.ErrVariableNotFound)
				c.EXPECT().GetVariable(mock.Anything, "web-config").Return(client.Variable{}, client.ErrVariableNotFound)
				c.EXPECT().Get(mock.Anything, "blog-cron").Return(client.Workload{}, client.ErrWorkloadNotFound)
				c.EXPECT().Get(mock.Anything, "blog-db").Return(client.Workload{}, client.ErrWorkloadNotFound)
				c.EXPECT().Get(mock.Anything, "blog-web").Return(client.Workload{}, client.ErrWorkloadNotFound)
				c.EXPECT().GetService(mock.Anything, "blog").Return(client.Service{}, client.ErrServiceNotFound)
			},
			ExpectErr: score.ErrUnowned,
		},
		{
			Name:    "adopts a resource it does not own when asked",
			Options: []score.ApplyOption{score.WithAdopt()},
			SetupMocks: func(c *MockClient) {
				expectRequirementsExist(c)
				c.EXPECT().GetVolume(mock.Anything, "blog-pgdata").Return(client.Volume{Name: "blog-pgdata"}, nil)
				c.EXPECT().GetVariable(mock.Anything, "motd").Return(client.Variable{}, client.ErrVariableNotFound)
				c.EXPECT().GetVariable(mock.Anything, "web-config").Return(client.Variable{}, client.ErrVariableNotFound)
				c.EXPECT().Get(mock.Anything, "blog-cron").Return(client.Workload{}, client.ErrWorkloadNotFound)
				c.EXPECT().Get(mock.Anything, "blog-db").Return(client.Workload{}, client.ErrWorkloadNotFound)
				c.EXPECT().Get(mock.Anything, "blog-web").Return(client.Workload{}, client.ErrWorkloadNotFound)
				c.EXPECT().GetService(mock.Anything, "blog").Return(client.Service{}, client.ErrServiceNotFound)

				c.EXPECT().ApplyVolume(mock.Anything, mock.Anything).Return(client.Volume{}, nil)
				c.EXPECT().SetVariable(mock.Anything, mock.Anything).Return(client.Variable{}, true, nil)
				c.EXPECT().Apply(mock.Anything, mock.Anything).Return(client.Workload{}, true, nil)
				c.EXPECT().ApplyService(mock.Anything, mock.Anything).Return(client.Service{}, nil)
			},
			Expected: score.Report{
				Applied: []score.Node{
					{Kind: score.KindVolume, Name: "blog-pgdata"},
					{Kind: score.KindVariable, Name: "motd"},
					{Kind: score.KindVariable, Name: "web-config"},
					{Kind: score.KindWorkload, Name: "blog-cron"},
					{Kind: score.KindWorkload, Name: "blog-db"},
					{Kind: score.KindWorkload, Name: "blog-web"},
					{Kind: score.KindService, Name: "blog"},
				},
			},
		},
		{
			Name: "stops at the first failure and reports what landed",
			SetupMocks: func(c *MockClient) {
				expectRequirementsExist(c)
				expectNothingExists(c)

				c.EXPECT().ApplyVolume(mock.Anything, mock.Anything).Return(client.Volume{}, nil)
				c.EXPECT().SetVariable(mock.Anything, mock.MatchedBy(func(variable manifest.Variable) bool {
					return variable.Name == "motd"
				})).Return(client.Variable{}, true, nil)
				c.EXPECT().SetVariable(mock.Anything, mock.MatchedBy(func(variable manifest.Variable) bool {
					return variable.Name == "web-config"
				})).Return(client.Variable{}, false, errors.New("boom"))
			},
			Expected: score.Report{
				Applied: []score.Node{
					{Kind: score.KindVolume, Name: "blog-pgdata"},
					{Kind: score.KindVariable, Name: "motd"},
				},
			},
			ExpectsErr: true,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			c := NewMockClient(t)
			tc.SetupMocks(c)

			actual, err := score.Apply(t.Context(), c, blog(t), tc.Options...)
			switch {
			case tc.ExpectErr != nil:
				assert.ErrorIs(t, err, tc.ExpectErr)
				assert.Zero(t, actual)
				return
			case tc.ExpectsErr:
				assert.Error(t, err)
			default:
				require.NoError(t, err)
			}

			assert.Equal(t, tc.Expected, actual)
		})
	}
}
