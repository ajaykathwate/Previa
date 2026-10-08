// Package outbound renders a repository's Outbound tab: the host:port
// destinations its owner approved for previews to reach.
package outbound

import (
	"strconv"
	"strings"
	"time"
)

// Destination is one approved destination.
type Destination struct {
	ID        int64
	Host      string
	Port      int
	CreatedAt time.Time
}

// Address is host:port.
func (d Destination) Address() string { return d.Host + ":" + strconv.Itoa(d.Port) }

func (d Destination) CreatedLabel() string { return d.CreatedAt.UTC().Format("2 Jan 2006, 15:04 UTC") }
func (d Destination) CreatedISO() string   { return d.CreatedAt.UTC().Format(time.RFC3339) }

// Page is the Outbound tab of one owned repository.
type Page struct {
	RepoID       int64
	FullName     string
	Available    bool  // GitHub access is current; adding requires it
	Enabled      bool  // the operator allows outbound connections
	Ports        []int // the ports the operator allows
	Limit        int
	Destinations []Destination
	Notice       string
	Error        string // why the last add was refused
	HostError    string
	PortError    string
	Host, Port   string // the rejected input, shown again
}

func (p Page) Owner() string {
	owner, _, _ := strings.Cut(p.FullName, "/")
	return owner
}

func (p Page) RepositoryName() string {
	_, name, found := strings.Cut(p.FullName, "/")
	if !found {
		return p.FullName
	}
	return name
}

// CanAdd reports whether a new destination can be approved now.
func (p Page) CanAdd() bool {
	return p.Enabled && p.Available && len(p.Destinations) < p.Limit && len(p.FreePorts()) > 0
}

// FreePorts are the allowed ports no destination uses yet.
func (p Page) FreePorts() []int {
	var free []int
	for _, port := range p.Ports {
		used := false
		for _, d := range p.Destinations {
			used = used || d.Port == port
		}
		if !used {
			free = append(free, port)
		}
	}
	return free
}

// PortLabel names a port with what usually listens on it.
func PortLabel(port int) string {
	names := map[int]string{
		1433: "SQL Server", 3306: "MySQL", 5432: "PostgreSQL", 5671: "AMQP over TLS", 5672: "AMQP",
		6379: "Redis", 6380: "Redis over TLS", 9092: "Kafka", 27017: "MongoDB",
	}
	if name, ok := names[port]; ok {
		return strconv.Itoa(port) + " (" + name + ")"
	}
	return strconv.Itoa(port)
}

// PortsLabel lists the allowed ports for a sentence.
func (p Page) PortsLabel() string {
	text := make([]string, len(p.Ports))
	for i, port := range p.Ports {
		text[i] = strconv.Itoa(port)
	}
	return strings.Join(text, ", ")
}

// CountLabel is "2 of 10".
func (p Page) CountLabel() string {
	return strconv.Itoa(len(p.Destinations)) + " of " + strconv.Itoa(p.Limit)
}
