package client_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/takt/internal/generated/api"
	"github.com/dsb-labs/takt/pkg/client"
)

func TestClient_SetVariable(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name          string
		Handler       http.HandlerFunc
		ExpectErr     func(error) bool
		ExpectCreated bool
		Assert        func(*testing.T, client.Variable)
	}{
		{
			Name: "creates a variable",
			Handler: func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPut, r.Method)
				assert.Equal(t, "/api/v1/variables/log-level", r.URL.Path)

				var body api.VariableSpec
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				assert.Equal(t, "debug", body.Value)

				writeJSON(t, w, http.StatusCreated,
					api.SetVariableResult{Variable: apiVariable("log-level", "debug")})
			},
			ExpectCreated: true,
			Assert: func(t *testing.T, variable client.Variable) {
				assert.Equal(t, "log-level", variable.Name)
				assert.Equal(t, "debug", variable.Value)
			},
		},
		{
			Name: "updates a variable",
			Handler: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, http.StatusOK,
					api.SetVariableResult{Variable: apiVariable("log-level", "debug")})
			},
			ExpectCreated: false,
		},
		{
			Name: "reports a name the server will not accept",
			Handler: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, http.StatusBadRequest, api.ErrorResponse{Error: "invalid variable"})
			},
			ExpectErr: client.IsBadRequest,
		},
		{
			Name: "reports a failure to store",
			Handler: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, http.StatusInternalServerError,
					api.ErrorResponse{Error: "failed to set variable"})
			},
			ExpectErr: func(err error) bool { return err != nil },
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			variable, created, err := newTestClient(t, tc.Handler).
				SetVariable(t.Context(), "log-level", "debug", nil)
			if tc.ExpectErr != nil {
				assert.True(t, tc.ExpectErr(err), "unexpected error: %v", err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.ExpectCreated, created)

			if tc.Assert != nil {
				tc.Assert(t, variable)
			}
		})
	}
}

func TestClient_GetVariable(t *testing.T) {
	t.Parallel()

	t.Run("returns the variable and its value", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodGet, r.Method)
			assert.Equal(t, "/api/v1/variables/log-level", r.URL.Path)

			stored := apiVariable("log-level", "debug")
			stored.UsedBy = new([]string{"example"})

			writeJSON(t, w, http.StatusOK, api.GetVariableResult{Variable: stored})
		})

		// The value comes back, which is where this parts company with GetSecret.
		variable, err := c.GetVariable(t.Context(), "log-level")
		require.NoError(t, err)
		assert.Equal(t, "log-level", variable.Name)
		assert.Equal(t, "debug", variable.Value)
		assert.Equal(t, []string{"example"}, variable.UsedBy)
	})

	t.Run("returns one holding nothing", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusOK, api.GetVariableResult{Variable: apiVariable("empty", "")})
		})

		variable, err := c.GetVariable(t.Context(), "empty")
		require.NoError(t, err)
		assert.Equal(t, "empty", variable.Name)
		assert.Empty(t, variable.Value)
	})

	t.Run("reports one that does not exist", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusNotFound, api.ErrorResponse{Error: `variable "nope" does not exist`})
		})

		_, err := c.GetVariable(t.Context(), "nope")
		assert.ErrorIs(t, err, client.ErrVariableNotFound)
	})

	t.Run("refuses a name that is not one path segment", func(t *testing.T) {
		// Go's HTTP client resolves dot segments before sending, so a name carrying one
		// would reach whichever endpoint the resolved path names.
		_, err := newTestClient(t, func(http.ResponseWriter, *http.Request) {
			t.Error("the server was reached with an unusable name")
		}).GetVariable(t.Context(), "../workloads")
		assert.ErrorIs(t, err, client.ErrInvalidVariableName)
	})
}

func TestClient_ListVariables(t *testing.T) {
	t.Parallel()

	t.Run("returns every variable with its value", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/api/v1/variables", r.URL.Path)

			writeJSON(t, w, http.StatusOK, api.ListVariablesResult{
				Variables: []api.Variable{
					apiVariable("db-host", "localhost"),
					apiVariable("log-level", "debug"),
				},
			})
		})

		variables, err := c.ListVariables(t.Context())
		require.NoError(t, err)
		require.Len(t, variables, 2)
		assert.Equal(t, "db-host", variables[0].Name)
		assert.Equal(t, "localhost", variables[0].Value)
	})

	t.Run("returns nothing when none are stored", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusOK, api.ListVariablesResult{})
		})

		variables, err := c.ListVariables(t.Context())
		require.NoError(t, err)
		assert.Empty(t, variables)
	})

	t.Run("sends queries as repeated parameters", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, []string{"$.labels.app=web", "$.labels.env=prod"}, r.URL.Query()["query"])

			writeJSON(t, w, http.StatusOK, api.ListVariablesResult{})
		})

		_, err := c.ListVariables(t.Context(), "$.labels.app=web", "$.labels.env=prod")
		require.NoError(t, err)
	})

	t.Run("reports a malformed query", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusBadRequest, api.ErrorResponse{Error: "invalid query"})
		})

		_, err := c.ListVariables(t.Context(), "nonsense")
		assert.True(t, client.IsBadRequest(err))
	})
}

func TestClient_DeleteVariable(t *testing.T) {
	t.Parallel()

	t.Run("removes a variable nothing reads", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodDelete, r.Method)
			assert.Empty(t, r.URL.Query().Get("force"))

			writeJSON(t, w, http.StatusOK, api.DeleteVariableResult{})
		})

		require.NoError(t, c.DeleteVariable(t.Context(), "log-level"))
	})

	t.Run("refuses one a workload reads", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusConflict,
				api.ErrorResponse{Error: "variable is in use: read by example"})
		})

		err := c.DeleteVariable(t.Context(), "log-level")
		require.ErrorIs(t, err, client.ErrVariableInUse)

		// The server's message names the workloads, so the caller can act on it.
		assert.Contains(t, err.Error(), "example")
	})

	t.Run("forces one a workload reads", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "true", r.URL.Query().Get("force"))

			writeJSON(t, w, http.StatusOK, api.DeleteVariableResult{})
		})

		require.NoError(t, c.DeleteVariable(t.Context(), "log-level", client.WithForceDeleteVariable()))
	})

	t.Run("reports one that does not exist", func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusNotFound, api.ErrorResponse{Error: `variable "nope" does not exist`})
		})

		assert.ErrorIs(t, c.DeleteVariable(t.Context(), "nope"), client.ErrVariableNotFound)
	})
}

func apiVariable(name, value string) api.Variable {
	now := time.Now().UTC().Truncate(time.Second)

	return api.Variable{
		Name:      name,
		Value:     value,
		CreatedAt: now,
		UpdatedAt: now,
	}
}
