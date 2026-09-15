package event

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The Fields type carries the values a reason's message is rendered from.
//
// It is one struct covering every reason rather than a type per reason, because
// the set of things an event names is small and reasons overlap in what they
// name. A reason uses the two or three fields its message needs and leaves the
// rest unset.
//
// The encoding is part of the stored row and half of the key events coalesce on,
// so field names may not change without orphaning the rows already written.
type Fields struct {
	// The image reference, expected hash, or other reference the event concerns.
	Reference string `json:"reference,omitempty"`
	// The name of another object the event concerns, such as the secret whose
	// change moved a workload's hash.
	Name string `json:"name,omitempty"`
	// The specification hash the workload is converging towards.
	Hash string `json:"hash,omitempty"`
	// The specification hash the workload was at, where the event reports a move.
	Previous string `json:"previous,omitempty"`
	// The version of the specification the event concerns.
	Version int `json:"version,omitempty"`
	// The ordinal of the instance the event concerns, counting from zero.
	Instance int `json:"instance,omitempty"`
	// The status an instance ended with. A pointer because zero is the code a
	// clean exit reports, so it has to be told apart from the field being unset.
	ExitCode *int `json:"exitCode,omitempty"`
	// How many times something has happened, such as the consecutive failures a
	// health check has reported. This counts within one sighting and is not the
	// count of sightings, which the stored event carries separately.
	Count int `json:"count,omitempty"`
	// How long the server is waiting before it tries again.
	Delay time.Duration `json:"delay,omitempty"`
	// The schedule expression the event concerns.
	Schedule string `json:"schedule,omitempty"`
	// The signal sent to the workload, where the event reports one.
	Signal string `json:"signal,omitempty"`
	// The host ports the event concerns.
	Ports []int `json:"ports,omitempty"`
	// What went wrong, where the event reports a failure.
	Error string `json:"error,omitempty"`
}

// Encode renders fields as the JSON stored against an event.
//
// No error is returned because Fields holds only types encoding/json can always
// encode, so the failure the encoder documents cannot arise here.
func Encode(fields Fields) []byte {
	data, err := json.Marshal(fields)
	if err != nil {
		// Unreachable, and a panic here would take down a converge pass over a
		// message. An empty object still coalesces and still renders, losing the
		// detail rather than the event.
		return []byte("{}")
	}

	return data
}

// Message renders the line an operator reads for an event.
//
// The reason is written for a machine to match on. This is the half written for
// a person, so it says what happened in plain words rather than restating the
// reason code.
//
// Rendering happens here rather than at the call site that recorded the event so
// the wording is not frozen into the stored row. A reason with no case below
// renders as itself, which is how an event written by a newer server reads on an
// older one rather than reading as nothing.
//
// Malformed data renders the reason alone. The row is already written by the time
// anything reads it, so there is no failure left to report and the reason is still
// worth showing.
func Message(reason Reason, data []byte) string {
	var fields Fields
	if len(data) > 0 {
		if err := json.Unmarshal(data, &fields); err != nil {
			return string(reason)
		}
	}

	switch reason {
	case ImagePulling:
		return fmt.Sprintf("Pulling image %s", fields.Reference)
	case RestartPaced:
		return fmt.Sprintf("Waiting %s before restart %d%s", fields.Delay, fields.Count, because(fields))
	case RestartGaveUp:
		return fmt.Sprintf("Gave up restarting after %d attempts", fields.Count)
	case ReferenceUnresolved:
		return fmt.Sprintf("Waiting for %s to resolve: %s", reference(fields), fields.Error)

	case SpecificationModified:
		return fmt.Sprintf("Specification changed to version %d", fields.Version)
	case PortsDrifted:
		return fmt.Sprintf("Replacing instance %d, which no longer publishes host %s %s",
			fields.Instance, plural("port", len(fields.Ports)), ports(fields))
	case HashMoved:
		return fmt.Sprintf("Replacing instance %d, the specification hash moved to %s", fields.Instance, short(fields.Hash))
	case SecretChanged:
		return fmt.Sprintf("Secret %s changed", fields.Name)
	case VariableChanged:
		return fmt.Sprintf("Variable %s changed", fields.Name)
	case AddressMoved:
		return fmt.Sprintf("Workload %s moved to a new address", fields.Name)
	case AddressRemoved:
		return fmt.Sprintf("Workload %s was deleted and can no longer be addressed", fields.Name)
	case HealthCheckFailing:
		return fmt.Sprintf("Health check failed %d times in a row: %s", fields.Count, fields.Error)
	case HealthCheckRecovered:
		return "Health check is passing again"
	case InstanceExited:
		return fmt.Sprintf("Instance %d exited with status %s", fields.Instance, exitCode(fields))
	case InstanceUnhealthy:
		return fmt.Sprintf("Replacing instance %d, which failed its health check %d times in a row: %s",
			fields.Instance, fields.Count, fields.Error)

	case Applied:
		return fmt.Sprintf("Specification applied at version %d", fields.Version)
	case Suspended:
		return "Workload suspended"
	case Resumed:
		return "Workload resumed"
	case RestartRequested:
		return "Restart requested"
	case Deleted:
		return "Workload marked for deletion"
	case InstanceStarted:
		return fmt.Sprintf("Started instance %d", fields.Instance)
	case InstanceRemoved:
		return fmt.Sprintf("Removed instance %d, which is no longer wanted", fields.Instance)
	case PortsAbandoned:
		return fmt.Sprintf("Gave up host %s %s", plural("port", len(fields.Ports)), ports(fields))
	case MountsRefreshed:
		return fmt.Sprintf("Mounted value %s changed, sent %s", fields.Name, fields.Signal)

	case RunStarted:
		return "Run started"
	case RunFinished:
		return fmt.Sprintf("Run finished with status %s", exitCode(fields))
	case OccurrenceSkipped:
		return "Skipped an occurrence, the run before it had not finished"
	case OccurrenceReplaced:
		return "An occurrence replaced a run that had not finished"
	case ScheduleInvalid:
		return fmt.Sprintf("Could not read schedule %q: %s", fields.Schedule, fields.Error)

	case ConvergeFailed:
		return fmt.Sprintf("Could not converge: %s", fields.Error)
	}

	return string(reason)
}

// short renders a hash as the leading characters an operator reads it by, which is
// how the rest of takt shows one.
//
// The stored event keeps the whole hash, so a caller matching on the data still has
// it. Only the line written for a person is shortened, which is the point of
// rendering at read time rather than at the call site.
func short(hash string) string {
	if len(hash) <= 12 {
		return hash
	}

	return hash[:12]
}

// reference renders what an event names, describing it instead where the event knows
// a reference could not be resolved without knowing which of several it was.
func reference(fields Fields) string {
	if fields.Reference == "" {
		return "a reference"
	}

	return fields.Reference
}

// because renders the failure an event reports as a clause on the end of its
// message, and nothing where the event recorded none.
func because(fields Fields) string {
	if fields.Error == "" {
		return ""
	}

	return ": " + fields.Error
}

// exitCode renders an instance's exit status, naming it as unknown when the event
// did not record one.
func exitCode(fields Fields) string {
	if fields.ExitCode == nil {
		return "unknown"
	}

	return strconv.Itoa(*fields.ExitCode)
}

// ports renders the host ports an event names as a comma-separated list.
func ports(fields Fields) string {
	rendered := make([]string, 0, len(fields.Ports))
	for _, port := range fields.Ports {
		rendered = append(rendered, strconv.Itoa(port))
	}

	return strings.Join(rendered, ", ")
}

// plural returns word suffixed for count, for the cases where an "s" is all that
// changes.
func plural(word string, count int) string {
	if count == 1 {
		return word
	}

	return word + "s"
}
