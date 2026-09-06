package wire

import (
	"github.com/dsb-labs/takt/internal/generated/api"
	"github.com/dsb-labs/takt/pkg/manifest"
)

// ToService maps a wire service specification onto the canonical shape.
//
// The defaults are resolved by the canonical package before the service is
// returned, so a service reaching takt over HTTP means what the same service
// written as a manifest file means.
func ToService(spec api.ServiceSpec) manifest.Service {
	out := manifest.Service{
		Version: spec.Version,
		Name:    spec.Name,
		Target: manifest.ServiceTarget{
			Labels: spec.Target.Labels,
			Port:   spec.Target.Port,
		},
	}

	if spec.Labels != nil {
		out.Labels = *spec.Labels
	}

	if spec.Target.Protocol != nil {
		out.Target.Protocol = manifest.Protocol(*spec.Target.Protocol)
	}

	out.Defaults()

	return out
}

// FromService maps a canonical service specification onto the wire shape, for a
// client submitting one.
func FromService(service manifest.Service) api.ServiceSpec {
	out := api.ServiceSpec{
		Version: service.Version,
		Name:    service.Name,
		Target: api.ServiceTarget{
			Labels: service.Target.Labels,
			Port:   service.Target.Port,
		},
	}

	if len(service.Labels) > 0 {
		labels := api.Labels(service.Labels)
		out.Labels = &labels
	}

	if service.Target.Protocol != "" {
		out.Target.Protocol = new(api.ServiceTargetProtocol(service.Target.Protocol))
	}

	return out
}
