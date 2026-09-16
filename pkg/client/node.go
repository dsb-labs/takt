package client

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/dsb-labs/takt/internal/generated/api"
)

type (
	// The Node type is the client-side view of the machine the server runs on:
	// what it is, and what it has.
	//
	// The capacity figures are the host's. Memory in use and the load averages
	// describe everything on the box, rather than what takt's workloads
	// consume, which each instance reports for itself.
	Node struct {
		// The host's name.
		Hostname string
		// The operating system the binary was built for.
		OS string
		// The processor architecture the binary was built for.
		Arch string
		// The kernel release.
		Kernel string
		// How many processors the host has.
		CPUs int
		// The version of the takt binary serving the request, which is what a
		// client checks before relying on something newer than the server.
		Version string
		// When the server process started.
		StartedAt time.Time
		// When the host booted.
		BootedAt time.Time
		// The host's memory.
		Memory NodeMemory
		// The host's load averages.
		Load NodeLoad
		// The filesystems takt writes to.
		Disks NodeDisks
	}

	// The NodeMemory type describes the host's memory, in bytes.
	NodeMemory struct {
		// How much memory the host has.
		Total int
		// How much of it is in use. This is the total less what the kernel
		// reports as available, so the page cache it reclaims before refusing
		// an allocation is not counted as spent.
		Used int
	}

	// The NodeLoad type carries the host's load averages.
	NodeLoad struct {
		// The load averaged over the last minute.
		One float64
		// The load averaged over the last five minutes.
		Five float64
		// The load averaged over the last fifteen minutes.
		Fifteen float64
	}

	// The NodeDisks type describes the filesystems under the directories takt
	// writes to. Both describe one filesystem unless the operator mounted
	// something at the volumes directory.
	NodeDisks struct {
		// The filesystem under the data directory.
		Data NodeDisk
		// The filesystem under the volumes directory.
		Volumes NodeDisk
	}

	// The NodeDisk type describes the filesystem under a directory, in bytes.
	NodeDisk struct {
		// The directory the figures describe, as configured. Reported even when
		// the directory has not been created yet.
		Path string
		// The size of the filesystem.
		Total int
		// How much of it an unprivileged writer can still use.
		Free int
	}
)

// GetNode reads the machine the server runs on.
func (c *Client) GetNode(ctx context.Context) (Node, error) {
	resp, err := c.api.GetNodeWithResponse(ctx)
	if err != nil {
		return Node{}, fmt.Errorf("failed to send the request: %w", err)
	}

	switch {
	case resp.JSON200 != nil:
		return newNode(resp.JSON200.Node), nil
	case resp.JSON500 != nil:
		return Node{}, newError(http.StatusInternalServerError, resp.JSON500)
	default:
		return Node{}, newError(resp.StatusCode(), nil)
	}
}

// newNode converts a node from the wire's shape to the client's.
func newNode(node api.Node) Node {
	return Node{
		Hostname:  node.Hostname,
		OS:        node.Os,
		Arch:      node.Arch,
		Kernel:    node.Kernel,
		CPUs:      node.Cpus,
		Version:   node.Version,
		StartedAt: node.StartedAt,
		BootedAt:  node.BootedAt,
		Memory: NodeMemory{
			Total: node.Memory.Total,
			Used:  node.Memory.Used,
		},
		Load: NodeLoad{
			One:     node.Load.One,
			Five:    node.Load.Five,
			Fifteen: node.Load.Fifteen,
		},
		Disks: NodeDisks{
			Data:    newNodeDisk(node.Disks.Data),
			Volumes: newNodeDisk(node.Disks.Volumes),
		},
	}
}

// newNodeDisk converts a filesystem reading from the wire's shape to the
// client's.
func newNodeDisk(disk api.NodeDisk) NodeDisk {
	return NodeDisk{
		Path:  disk.Path,
		Total: disk.Total,
		Free:  disk.Free,
	}
}
