package e2e_test

import (
	"os"
	"os/exec"
	"strings"
	"time"
)

// TestUnreferencedImagePruned covers the claim that an image goes once no workload
// names it and no container uses it, and stays while either holds.
//
// Pruning reaches every image on the daemon, so the test runs only where an
// environment variable says the daemon's images are disposable. The e2e workflow
// sets it, and a developer's daemon is left alone.
func (s *Suite) TestUnreferencedImagePruned() {
	if os.Getenv("TAKT_E2E_PRUNE") == "" {
		s.T().Skip("pruning removes every unreferenced image on the daemon: set TAKT_E2E_PRUNE to allow it")
	}

	name := s.workloadName()
	s.T().Cleanup(func() { s.cleanup(name) })

	// Three images of their own, since a tag on the test image would share its
	// identifier and be held by every workload naming it. With the delay at zero
	// an image goes on the first prune after nothing references it, so each is
	// held by a container of its own until the workload names it.
	one, two, three := s.buildImage(name, "one"), s.buildImage(name, "two"), s.buildImage(name, "three")
	holders := map[string]string{two: s.holdImage(two), three: s.holdImage(three)}

	directory := s.T().TempDir()
	s.restart(withDataDirectory(directory))

	spec := s.containerSpec(name)
	spec.Container.Image = one

	_, _, err := s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)
	first := s.awaitInstance(name)

	// Applied before the holder goes, so the image is never unreferenced.
	spec.Container.Image = two

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)
	s.release(holders[two])
	s.awaitInstanceOtherThan(name, first)

	// The prune runs on a server's first pass. The first image is not named
	// any more, but the attempt takt retained for the workload's previous
	// output still uses it, and a container holding an image keeps it.
	s.restart(withDataDirectory(directory), withPrune())
	s.awaitInstance(name)
	s.True(s.imageExists(one), "the image the retained container uses was removed")

	second := s.instanceID(name)
	spec.Container.Image = three

	_, _, err = s.client.Apply(s.ctx(), spec)
	s.Require().NoError(err)
	s.release(holders[three])
	s.awaitInstanceOtherThan(name, second)

	// Now the retained container is the second attempt's, and nothing holds the
	// first image at all.
	s.restart(withDataDirectory(directory), withPrune())
	s.awaitInstance(name)

	s.Require().Eventually(func() bool {
		return !s.imageExists(one)
	}, convergeTimeout, 500*time.Millisecond, "the unreferenced image was never removed")

	s.True(s.imageExists(two), "the image the retained container uses was removed")
	s.True(s.imageExists(three), "the image the workload names was removed")
	s.Len(s.containers(name), 2, "the prune removed a container")
}

// buildImage builds an image of its own from the test image, tagged for the named
// workload, and removes it once the test ends. A layer is added so that each build
// has an identifier of its own rather than sharing the test image's.
func (s *Suite) buildImage(workload, tag string) string {
	s.T().Helper()

	ref := "takt-e2e/" + workload + ":" + tag

	build := exec.Command("docker", "build", "--quiet", "--tag", ref, "-")
	build.Stdin = strings.NewReader("FROM " + testImage + "\nRUN echo " + tag + " > /takt-e2e\n")

	out, err := build.CombinedOutput()
	s.Require().NoError(err, string(out))

	s.T().Cleanup(func() { _ = exec.Command("docker", "rmi", "--force", ref).Run() })

	return ref
}

// holdImage creates a container from the given image without starting it, so the
// image is held the way any container holds one, and returns the container's
// identifier. The container is removed once the test ends if it is still there.
func (s *Suite) holdImage(ref string) string {
	s.T().Helper()

	out, err := exec.Command("docker", "create", ref).Output()
	s.Require().NoError(err)

	id := strings.TrimSpace(string(out))
	s.T().Cleanup(func() { _ = exec.Command("docker", "rm", "--force", id).Run() })

	return id
}

// release removes a container holdImage created, so the image it held is held
// by nothing.
func (s *Suite) release(id string) {
	s.T().Helper()

	s.Require().NoError(exec.Command("docker", "rm", "--force", id).Run())
}

// imageExists reports whether the daemon holds an image under the given reference.
func (s *Suite) imageExists(ref string) bool {
	out, err := exec.Command("docker", "images", "--quiet", ref).Output()
	s.Require().NoError(err)

	return strings.TrimSpace(string(out)) != ""
}
