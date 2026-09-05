package service_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsb-labs/orca/internal/server/database"
	"github.com/dsb-labs/orca/internal/server/service"
)

func TestVolumeService_Create(t *testing.T) {
	t.Parallel()

	t.Run("creates the volume and the directory backing it", func(t *testing.T) {
		t.Parallel()

		repo := NewMockVolumeRepository(t)
		repo.EXPECT().Insert(mock.Anything, database.Volume{Name: "example-data", Labels: map[string]string{"app": "web"}}).
			Return(database.Volume{
				ID:        testVolumeID,
				Name:      "example-data",
				Labels:    map[string]string{"app": "web"},
				CreatedAt: time.Now(),
			}, nil).Once()

		svc, root := newVolumeService(t, repo)

		volume, err := svc.Create(t.Context(), "example-data", map[string]string{"app": "web"})
		require.NoError(t, err)

		assert.Equal(t, "example-data", volume.Name)

		// The directory is named for the identifier rather than the name, which is
		// the operator's handle and never a path component.
		assert.Equal(t, filepath.Join(root, "volumes", testVolumeID), volume.Path)
		assert.Equal(t, map[string]string{"app": "web"}, volume.Labels)

		info, err := os.Stat(volume.Path)
		require.NoError(t, err, "the directory backing the volume was not created")
		assert.True(t, info.IsDir())
		assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(),
			"the volume is readable by more than the user running the server")
	})

	t.Run("refuses a name orca would not accept", func(t *testing.T) {
		t.Parallel()

		// Refused before anything is written, so a name that could never be a volume
		// leaves no row and no directory. The repository expects no calls.
		repo := NewMockVolumeRepository(t)
		svc, _ := newVolumeService(t, repo)

		for _, name := range []string{"Example_Data", "", "-leading", "trailing-", "a/b", ".."} {
			_, err := svc.Create(t.Context(), name, nil)
			assert.ErrorIs(t, err, service.ErrInvalidVolume, "accepted the name %q", name)
		}
	})

	t.Run("reports a name another volume holds", func(t *testing.T) {
		t.Parallel()

		// A volume holds data, so handing a caller who meant a new name somebody
		// else's storage is worse than failing.
		repo := NewMockVolumeRepository(t)
		repo.EXPECT().Insert(mock.Anything, mock.Anything).
			Return(database.Volume{}, database.ErrVolumeExists).Once()

		svc, _ := newVolumeService(t, repo)

		_, err := svc.Create(t.Context(), "example-data", nil)
		assert.ErrorIs(t, err, service.ErrVolumeExists)
	})

	t.Run("removes the row when the directory cannot be created", func(t *testing.T) {
		t.Parallel()

		// A row naming a volume whose directory does not exist would be reported as a
		// volume nothing can reach, and nothing later creates the directory. Better to
		// leave nothing behind.
		repo := NewMockVolumeRepository(t)
		repo.EXPECT().Insert(mock.Anything, mock.Anything).
			Return(database.Volume{ID: testVolumeID, Name: "example-data"}, nil).Once()
		repo.EXPECT().Delete(mock.Anything, "example-data").Return(nil).Once()

		// A file where the volumes directory belongs, so creating one beneath it fails
		// the way a full or read-only disk would.
		root := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(root, "volumes"), nil, 0o600))

		svc := service.NewVolumeService(service.VolumeServiceConfig{
			Logger:  newTestLogger(t),
			Volumes: repo,
			Root:    filepath.Join(root, "volumes"),
		})

		_, err := svc.Create(t.Context(), "example-data", nil)
		assert.Error(t, err)
	})
}

func TestVolumeService_Delete(t *testing.T) {
	t.Parallel()

	t.Run("removes the volume and its contents", func(t *testing.T) {
		t.Parallel()

		// The only thing in orca that destroys stored data, so the directory going is
		// worth asserting rather than assuming.
		repo := NewMockVolumeRepository(t)
		repo.EXPECT().Insert(mock.Anything, mock.Anything).
			Return(database.Volume{ID: testVolumeID, Name: "example-data"}, nil).Once()
		repo.EXPECT().Get(mock.Anything, "example-data").
			Return(database.Volume{ID: testVolumeID, Name: "example-data"}, nil).Once()
		repo.EXPECT().UsedBy(mock.Anything, "example-data").Return(nil, nil).Once()
		repo.EXPECT().Delete(mock.Anything, "example-data").Return(nil).Once()

		svc, _ := newVolumeService(t, repo)

		volume, err := svc.Create(t.Context(), "example-data", nil)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(volume.Path, "file"), []byte("data"), 0o600))

		require.NoError(t, svc.Delete(t.Context(), "example-data", false))

		_, err = os.Stat(volume.Path)
		assert.True(t, os.IsNotExist(err), "the volume's directory outlived the volume")
	})

	t.Run("names the fix when the contents cannot be removed", func(t *testing.T) {
		t.Parallel()

		// A container writes to a volume as whatever user its image names, and the
		// server's user cannot remove another user's files. The failure has to name
		// what grants that, because nothing else about a permission error says why a
		// directory orca created cannot be removed.
		repo := NewMockVolumeRepository(t)
		repo.EXPECT().Insert(mock.Anything, mock.Anything).
			Return(database.Volume{ID: testVolumeID, Name: "example-data"}, nil).Once()
		repo.EXPECT().Get(mock.Anything, "example-data").
			Return(database.Volume{ID: testVolumeID, Name: "example-data"}, nil).Once()
		repo.EXPECT().UsedBy(mock.Anything, "example-data").Return(nil, nil).Once()

		svc, _ := newVolumeService(t, repo)

		volume, err := svc.Create(t.Context(), "example-data", nil)
		require.NoError(t, err)

		// A directory this user cannot read stands in for one owned by another user,
		// which an unprivileged test cannot make.
		locked := filepath.Join(volume.Path, "pgdata")
		require.NoError(t, os.Mkdir(locked, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(locked, "file"), []byte("data"), 0o600))
		require.NoError(t, os.Chmod(locked, 0o000))
		t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

		if _, err := os.ReadDir(locked); err == nil {
			t.Skip("this user reads a directory with no permissions, so removal would succeed")
		}

		err = svc.Delete(t.Context(), "example-data", false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "CAP_DAC_OVERRIDE",
			"a permission failure did not say what grants removing another user's files")
	})

	tt := []struct {
		Name       string
		Force      bool
		SetupMocks func(*MockVolumeRepository)
		ExpectErr  error
		Contains   []string
	}{
		{
			Name: "refuses a volume a workload mounts",
			SetupMocks: func(repo *MockVolumeRepository) {
				repo.EXPECT().Get(mock.Anything, "example-data").
					Return(database.Volume{ID: testVolumeID, Name: "example-data"}, nil).Once()
				repo.EXPECT().UsedBy(mock.Anything, "example-data").
					Return([]string{"alpha", "bravo"}, nil).Once()
			},
			ExpectErr: service.ErrVolumeInUse,
			// Named, because the caller's next question is which workloads.
			Contains: []string{"alpha", "bravo"},
		},
		{
			Name:  "removes a volume in use when forced",
			Force: true,
			SetupMocks: func(repo *MockVolumeRepository) {
				repo.EXPECT().Get(mock.Anything, "example-data").
					Return(database.Volume{ID: testVolumeID, Name: "example-data"}, nil).Once()
				repo.EXPECT().UsedBy(mock.Anything, "example-data").Return([]string{"alpha"}, nil).Once()
				repo.EXPECT().Delete(mock.Anything, "example-data").Return(nil).Once()
			},
		},
		{
			Name: "reports a volume that does not exist",
			SetupMocks: func(repo *MockVolumeRepository) {
				repo.EXPECT().Get(mock.Anything, "example-data").
					Return(database.Volume{}, database.ErrVolumeNotFound).Once()
			},
			ExpectErr: service.ErrVolumeNotFound,
		},
		{
			Name: "keeps the row when the directory cannot be removed",
			SetupMocks: func(repo *MockVolumeRepository) {
				// The row is removed last, so a failure here leaves a volume that can
				// be deleted again. A directory with no row would be unreachable and
				// unnamed, and removal is only ever asked for by name.
				repo.EXPECT().Get(mock.Anything, "example-data").
					Return(database.Volume{ID: "not-an-identifier", Name: "example-data"}, nil).Once()
				repo.EXPECT().UsedBy(mock.Anything, "example-data").Return(nil, nil).Once()
			},
			ExpectErr: service.ErrInvalidVolume,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()

			repo := NewMockVolumeRepository(t)
			tc.SetupMocks(repo)

			svc, _ := newVolumeService(t, repo)

			err := svc.Delete(t.Context(), "example-data", tc.Force)
			if tc.ExpectErr == nil {
				assert.NoError(t, err)

				return
			}

			require.ErrorIs(t, err, tc.ExpectErr)

			for _, want := range tc.Contains {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}

func TestVolumeService_Get(t *testing.T) {
	t.Parallel()

	created := time.Now().UTC().Truncate(time.Second)

	tt := []struct {
		Name       string
		SetupMocks func(*MockVolumeRepository)
		ExpectErr  error
		ExpectsErr bool
		Assert     func(*testing.T, service.Volume, string)
	}{
		{
			Name: "returns the volume and what mounts it",
			SetupMocks: func(repo *MockVolumeRepository) {
				repo.EXPECT().Get(mock.Anything, "example-data").
					Return(database.Volume{ID: testVolumeID, Name: "example-data", CreatedAt: created}, nil).Once()
				repo.EXPECT().UsedBy(mock.Anything, "example-data").Return([]string{"alpha"}, nil).Once()
			},
			Assert: func(t *testing.T, volume service.Volume, root string) {
				assert.Equal(t, "example-data", volume.Name)
				assert.Equal(t, filepath.Join(root, "volumes", testVolumeID), volume.Path)
				assert.Equal(t, []string{"alpha"}, volume.UsedBy)
				assert.Equal(t, created, volume.CreatedAt)
			},
		},
		{
			Name: "reports a volume nothing mounts",
			SetupMocks: func(repo *MockVolumeRepository) {
				repo.EXPECT().Get(mock.Anything, "example-data").
					Return(database.Volume{ID: testVolumeID, Name: "example-data"}, nil).Once()
				repo.EXPECT().UsedBy(mock.Anything, "example-data").Return(nil, nil).Once()
			},
			Assert: func(t *testing.T, volume service.Volume, _ string) {
				assert.Empty(t, volume.UsedBy)
			},
		},
		{
			Name: "reports a volume that does not exist",
			SetupMocks: func(repo *MockVolumeRepository) {
				repo.EXPECT().Get(mock.Anything, "example-data").
					Return(database.Volume{}, database.ErrVolumeNotFound).Once()
			},
			ExpectErr: service.ErrVolumeNotFound,
		},
		{
			Name: "reports a failure reading what mounts it",
			SetupMocks: func(repo *MockVolumeRepository) {
				repo.EXPECT().Get(mock.Anything, "example-data").
					Return(database.Volume{ID: testVolumeID, Name: "example-data"}, nil).Once()
				repo.EXPECT().UsedBy(mock.Anything, "example-data").
					Return(nil, errors.New("database is locked")).Once()
			},
			ExpectsErr: true,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()

			repo := NewMockVolumeRepository(t)
			tc.SetupMocks(repo)

			svc, root := newVolumeService(t, repo)

			volume, err := svc.Get(t.Context(), "example-data")
			switch {
			case tc.ExpectsErr:
				assert.Error(t, err)

				return
			case tc.ExpectErr != nil:
				assert.ErrorIs(t, err, tc.ExpectErr)

				return
			}

			require.NoError(t, err)
			tc.Assert(t, volume, root)
		})
	}
}

func TestVolumeService_List(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name       string
		SetupMocks func(*MockVolumeRepository)
		ExpectsErr bool
		Assert     func(*testing.T, []service.Volume)
	}{
		{
			Name: "returns every volume with what mounts it",
			SetupMocks: func(repo *MockVolumeRepository) {
				repo.EXPECT().List(mock.Anything).Return([]database.Volume{
					{ID: testVolumeID, Name: "alpha"},
					{ID: otherVolumeID, Name: "bravo"},
				}, nil).Once()
				repo.EXPECT().UsedBy(mock.Anything, "alpha").Return([]string{"one"}, nil).Once()
				repo.EXPECT().UsedBy(mock.Anything, "bravo").Return(nil, nil).Once()
			},
			Assert: func(t *testing.T, volumes []service.Volume) {
				require.Len(t, volumes, 2)

				assert.Equal(t, "alpha", volumes[0].Name)
				assert.Equal(t, []string{"one"}, volumes[0].UsedBy)
				assert.Equal(t, "bravo", volumes[1].Name)
				assert.Empty(t, volumes[1].UsedBy)
			},
		},
		{
			Name: "returns nothing when no volume exists",
			SetupMocks: func(repo *MockVolumeRepository) {
				repo.EXPECT().List(mock.Anything).Return(nil, nil).Once()
			},
			Assert: func(t *testing.T, volumes []service.Volume) {
				assert.Empty(t, volumes)
			},
		},
		{
			Name: "reports a failure reading what mounts a volume",
			SetupMocks: func(repo *MockVolumeRepository) {
				repo.EXPECT().List(mock.Anything).
					Return([]database.Volume{{ID: testVolumeID, Name: "alpha"}}, nil).Once()
				repo.EXPECT().UsedBy(mock.Anything, "alpha").
					Return(nil, errors.New("database is locked")).Once()
			},
			ExpectsErr: true,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()

			repo := NewMockVolumeRepository(t)
			tc.SetupMocks(repo)

			svc, _ := newVolumeService(t, repo)

			volumes, err := svc.List(t.Context())
			if tc.ExpectsErr {
				assert.Error(t, err)

				return
			}

			require.NoError(t, err)
			tc.Assert(t, volumes)
		})
	}
}

func TestVolumeService_List_Queries(t *testing.T) {
	t.Parallel()

	t.Run("passes parsed queries to the repository", func(t *testing.T) {
		t.Parallel()

		repo := NewMockVolumeRepository(t)

		repo.EXPECT().List(mock.Anything, []database.Query{{Path: "$.labels.app", Value: "web"}}).
			Return([]database.Volume{{ID: testVolumeID, Name: "alpha"}}, nil).Once()
		repo.EXPECT().UsedBy(mock.Anything, "alpha").Return(nil, nil).Once()

		svc, _ := newVolumeService(t, repo)

		got, err := svc.List(t.Context(), "$.labels.app=web")
		require.NoError(t, err)
		assert.Len(t, got, 1)
	})

	t.Run("rejects a query that is not path=value", func(t *testing.T) {
		t.Parallel()

		svc, _ := newVolumeService(t, NewMockVolumeRepository(t))

		_, err := svc.List(t.Context(), "$.labels.app")
		assert.ErrorIs(t, err, service.ErrInvalidQuery)
	})

	t.Run("reports a path the repository cannot parse", func(t *testing.T) {
		t.Parallel()

		repo := NewMockVolumeRepository(t)

		repo.EXPECT().List(mock.Anything, mock.Anything).
			Return(nil, database.ErrInvalidQueryPath).Once()

		svc, _ := newVolumeService(t, repo)

		// A bad path is the caller's mistake, so it must not surface as a server
		// failure.
		_, err := svc.List(t.Context(), "nonsense=web")
		assert.ErrorIs(t, err, service.ErrInvalidQuery)
	})
}

func TestVolumeService_Path(t *testing.T) {
	t.Parallel()

	t.Run("returns where the volume's data is", func(t *testing.T) {
		t.Parallel()

		// This is what resolves a mount when a workload is applied, so it answers with
		// a path rather than making the caller understand the layout.
		repo := NewMockVolumeRepository(t)
		repo.EXPECT().Get(mock.Anything, "example-data").
			Return(database.Volume{ID: testVolumeID, Name: "example-data"}, nil).Once()

		svc, root := newVolumeService(t, repo)

		path, err := svc.Path(t.Context(), "example-data")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(root, "volumes", testVolumeID), path)
	})

	t.Run("reports a volume that does not exist", func(t *testing.T) {
		t.Parallel()

		// What makes applying a workload that mounts an unknown volume fail, rather
		// than the workload starting and writing somewhere unexpected.
		repo := NewMockVolumeRepository(t)
		repo.EXPECT().Get(mock.Anything, "nope").
			Return(database.Volume{}, database.ErrVolumeNotFound).Once()

		svc, _ := newVolumeService(t, repo)

		_, err := svc.Path(t.Context(), "nope")
		assert.ErrorIs(t, err, service.ErrVolumeNotFound)
	})

	t.Run("refuses an identifier that is not usable as a directory", func(t *testing.T) {
		t.Parallel()

		// The identifier comes from orca's own database rather than from a caller, so
		// this guards against orca's mistakes rather than someone else's. It is checked
		// because the path it builds is one directories are removed from.
		for _, id := range []string{"../escape", "..", "", "short", "UPPERCASE0000000000A"} {
			repo := NewMockVolumeRepository(t)
			repo.EXPECT().Get(mock.Anything, "example-data").
				Return(database.Volume{ID: id, Name: "example-data"}, nil).Once()

			svc, _ := newVolumeService(t, repo)

			_, err := svc.Path(t.Context(), "example-data")
			assert.ErrorIs(t, err, service.ErrInvalidVolume, "accepted the identifier %q", id)
		}
	})
}

// The identifiers orca assigns are xid values: twenty lowercase alphanumeric
// characters.
const (
	testVolumeID  = "cvhs0dq0kqj4c9r8m1a0"
	otherVolumeID = "cvhs0dq0kqj4c9r8m1a1"
)

// newVolumeService returns a volume service alongside the data directory it keeps
// volumes under.
func TestVolumeService_Update(t *testing.T) {
	t.Parallel()

	t.Run("replaces the labels", func(t *testing.T) {
		t.Parallel()

		// The only thing a volume has to change. Its name identifies it, its
		// directory is named for its identifier, and its contents are the workloads'.
		repo := NewMockVolumeRepository(t)
		repo.EXPECT().Update(mock.Anything, database.Volume{Name: "example-data", Labels: map[string]string{"app": "api"}}).
			Return(database.Volume{
				ID:     testVolumeID,
				Name:   "example-data",
				Labels: map[string]string{"app": "api"},
			}, nil).Once()
		repo.EXPECT().UsedBy(mock.Anything, "example-data").Return(nil, nil).Once()

		svc, _ := newVolumeService(t, repo)

		volume, err := svc.Update(t.Context(), "example-data", map[string]string{"app": "api"})
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"app": "api"}, volume.Labels)
	})

	t.Run("reports a volume that does not exist", func(t *testing.T) {
		t.Parallel()

		repo := NewMockVolumeRepository(t)
		repo.EXPECT().Update(mock.Anything, mock.Anything).
			Return(database.Volume{}, database.ErrVolumeNotFound).Once()

		svc, _ := newVolumeService(t, repo)

		_, err := svc.Update(t.Context(), "example-data", nil)
		assert.ErrorIs(t, err, service.ErrVolumeNotFound)
	})

	t.Run("refuses a label orca reserves for itself", func(t *testing.T) {
		t.Parallel()

		// Refused before the repository is reached, which the mock asserts by
		// expecting nothing.
		svc, _ := newVolumeService(t, NewMockVolumeRepository(t))

		_, err := svc.Update(t.Context(), "example-data", map[string]string{"orca.workload": "sneaky"})
		assert.ErrorIs(t, err, service.ErrInvalidVolume)
	})
}

func newVolumeService(t *testing.T, repo service.VolumeRepository) (*service.VolumeService, string) {
	t.Helper()

	root := t.TempDir()

	return service.NewVolumeService(service.VolumeServiceConfig{
		Logger:  newTestLogger(t),
		Volumes: repo,
		Root:    filepath.Join(root, "volumes"),
	}), root
}
