package api

import (
	"context"
	"log/slog"

	"github.com/dsb-labs/takt/internal/generated/api"
	"github.com/dsb-labs/takt/internal/server/service"
)

type (
	// The NodeService interface describes how the API reads the machine the
	// server runs on.
	NodeService interface {
		// Get should report the node as it stands now.
		Get() (service.Node, error)
	}

	// The NodeAPI type exposes HTTP endpoints describing the node.
	NodeAPI struct {
		logger *slog.Logger
		node   NodeService
	}

	// The NodeAPIConfig type contains fields used to construct a NodeAPI.
	NodeAPIConfig struct {
		// The logger used to record failures the response deliberately doesn't
		// describe.
		Logger *slog.Logger
		// The service reading the node.
		Node NodeService
	}
)

// NewNodeAPI returns a new instance of the NodeAPI type.
func NewNodeAPI(config NodeAPIConfig) *NodeAPI {
	return &NodeAPI{
		logger: config.Logger.With("component", "api"),
		node:   config.Node,
	}
}

// GetNode returns the machine the server runs on.
func (a *NodeAPI) GetNode(_ context.Context, _ api.GetNodeRequestObject) (api.GetNodeResponseObject, error) {
	node, err := a.node.Get()
	if err != nil {
		return api.GetNode500JSONResponse{
			Error: internalError(a.logger, "get node", err),
		}, nil
	}

	return api.GetNode200JSONResponse{Node: newNode(node)}, nil
}

// newNode converts a node from the service's shape to the wire's.
func newNode(node service.Node) api.Node {
	return api.Node{
		Hostname:  node.Hostname,
		Os:        node.OS,
		Arch:      node.Arch,
		Kernel:    node.Kernel,
		Cpus:      node.CPUs,
		Version:   node.Version,
		StartedAt: node.StartedAt,
		BootedAt:  node.BootedAt,
		Memory: api.NodeMemory{
			Total: node.Memory.Total,
			Used:  node.Memory.Used,
		},
		Load: api.NodeLoad{
			One:     node.Load.One,
			Five:    node.Load.Five,
			Fifteen: node.Load.Fifteen,
		},
		Disks: api.NodeDisks{
			Data:    newNodeDisk(node.Disks.Data),
			Volumes: newNodeDisk(node.Disks.Volumes),
		},
	}
}

// newNodeDisk converts a filesystem reading from the service's shape to the
// wire's.
func newNodeDisk(disk service.NodeDisk) api.NodeDisk {
	return api.NodeDisk{
		Path:  disk.Path,
		Total: disk.Total,
		Free:  disk.Free,
	}
}
