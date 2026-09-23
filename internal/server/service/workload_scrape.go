package service

import (
	"cmp"
	"context"
	"net"
	"slices"
	"strconv"
	"strings"

	"github.com/dsb-labs/takt/pkg/manifest"
)

type (
	// The ScrapeTarget type is one target group in the shape prometheus's
	// http_sd_configs reads: the addresses to scrape, and the labels attached
	// to every series they produce.
	ScrapeTarget struct {
		Targets []string
		Labels  map[string]string
	}
)

// The labels the scrape endpoint consumes rather than emits: the opt-in, and
// the port selection only takt can resolve. Everything else under the
// prometheus. namespace passes through with the prefix stripped, so a
// workload sets any label of its own without takt modelling it.
//
// Prometheus's meta labels are the exception, because a takt label key
// cannot start a segment with an underscore: path and scheme are the
// spellings a manifest may write, emitted under the names prometheus reads.
const (
	scrapeLabel       = "prometheus.scrape"
	scrapePortLabel   = "prometheus.port"
	scrapePathLabel   = "prometheus.path"
	scrapeSchemeLabel = "prometheus.scheme"
	scrapeNamespace   = "prometheus."
	scrapeQuery       = `$.labels."prometheus.scrape"=true`
)

// ScrapeTargets reports the workloads that opted into prometheus scraping as
// http_sd target groups, one group per workload with a target per instance.
//
// Targets come from the port allocations rather than from observation, so an
// instance that is failing to start is still a target: prometheus reports it
// down rather than never hearing of it, which is the signal an alert needs.
// Discovery describes desired state, and the scrape itself is the health
// check. A suspended workload is left out — the operator said stop, and a
// down target nobody can fix is noise — and so is a scheduled one, which is
// down between runs by design.
//
// The prometheus.port label selects which published port is scraped, by name
// or by number as a health check selects one, defaulted when the workload
// publishes exactly one. A workload whose selection matches nothing
// contributes nothing: labels are free text validated long before anything
// knows they are scrape configuration, so a bad one is the operator's to
// notice.
//
// The job label defaults to the workload's name, so targets do not collapse
// into the scrape configuration's single job, and takt_workload carries the
// workload's identity through a job override.
func (s *WorkloadService) ScrapeTargets(ctx context.Context) ([]ScrapeTarget, error) {
	workloads, err := s.List(ctx, scrapeQuery)
	if err != nil {
		return nil, err
	}

	var groups []ScrapeTarget
	for _, workload := range workloads {
		// The query already selected on the label, but the loop re-checks it:
		// this method's contract is the label's, not the query syntax's.
		if workload.Labels[scrapeLabel] != "true" {
			continue
		}

		if workload.Deleting || workload.Suspended || workload.Spec.Schedule != nil {
			continue
		}

		entry, ok := scrapePort(workload.Spec.Ports, workload.Labels[scrapePortLabel])
		if !ok {
			continue
		}

		targets := scrapeAddresses(s.address, workload.Ports, entry)
		if len(targets) == 0 {
			continue
		}

		groups = append(groups, ScrapeTarget{
			Targets: targets,
			Labels:  scrapeLabels(workload.Name, workload.Labels),
		})
	}

	// Ordered so that the same fleet always reads back the same way, whatever
	// order the workloads were listed in.
	slices.SortFunc(groups, func(a, b ScrapeTarget) int {
		return cmp.Compare(a.Labels["takt_workload"], b.Labels["takt_workload"])
	})

	return groups, nil
}

// scrapePort selects which of the workload's ports is scraped: the one the
// reference names, or the only one when the workload publishes exactly one
// and the reference is empty.
func scrapePort(ports []manifest.Port, ref string) (manifest.Port, bool) {
	if ref == "" {
		if len(ports) == 1 {
			return ports[0], true
		}

		return manifest.Port{}, false
	}

	reference := manifest.PortRef(ref)
	for _, port := range ports {
		if reference.Matches(port.Name, port.To) {
			return port, true
		}
	}

	return manifest.Port{}, false
}

// scrapeAddresses joins the advertised address with the host port each
// instance holds for the selected port, ordered by instance.
func scrapeAddresses(address string, ports []ResolvedPort, entry manifest.Port) []string {
	protocol := entry.Protocol
	if protocol == "" {
		protocol = manifest.ProtocolTCP
	}

	matched := slices.DeleteFunc(slices.Clone(ports), func(port ResolvedPort) bool {
		return port.To != entry.To || port.Protocol != protocol
	})

	slices.SortFunc(matched, func(a, b ResolvedPort) int {
		return cmp.Compare(a.Instance, b.Instance)
	})

	targets := make([]string, 0, len(matched))
	for _, port := range matched {
		targets = append(targets, net.JoinHostPort(address, strconv.Itoa(port.From)))
	}

	return targets
}

// scrapeLabels builds a group's labels: the job and workload defaults, and
// then whatever else the prometheus. namespace carries, prefix stripped, so a
// prometheus.job label overrides the default the way any passthrough wins.
func scrapeLabels(name string, labels map[string]string) map[string]string {
	out := map[string]string{
		"job":           name,
		"takt_workload": name,
	}

	for key, value := range labels {
		switch key {
		case scrapeLabel, scrapePortLabel:
			continue
		case scrapePathLabel:
			out["__metrics_path__"] = value

			continue
		case scrapeSchemeLabel:
			out["__scheme__"] = value

			continue
		}

		if rest, ok := strings.CutPrefix(key, scrapeNamespace); ok && rest != "" {
			out[rest] = value
		}
	}

	return out
}
