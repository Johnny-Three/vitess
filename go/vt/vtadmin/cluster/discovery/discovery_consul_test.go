package discovery

import (
	"context"
	"sort"
	"testing"
	"text/template"

	consul "github.com/hashicorp/consul/api"
	"github.com/stretchr/testify/assert"

	vtadminpb "vitess.io/vitess/go/vt/proto/vtadmin"
)

type fakeConsulClient struct {
	health *fakeConsulHealth
}

func (c *fakeConsulClient) Health() ConsulHealth { return c.health }

type fakeConsulHealth struct {
	entries map[string][]*consul.ServiceEntry
}

func (health *fakeConsulHealth) ServiceMultipleTags(service string, tags []string, passingOnly bool, q *consul.QueryOptions) ([]*consul.ServiceEntry, *consul.QueryMeta, error) { // nolint:lll
	if health.entries == nil {
		return nil, nil, assert.AnError
	}

	sort.Strings(tags)

	serviceEntries, ok := health.entries[service]
	if !ok {
		return []*consul.ServiceEntry{}, nil, nil
	}

	filterByTags := func(etags []string) bool {
		sort.Strings(etags)

		for _, tag := range tags {
			i := sort.SearchStrings(etags, tag)
			if i >= len(etags) || etags[i] != tag {
				return false
			}
		}

		return true
	}

	filteredEntries := make([]*consul.ServiceEntry, 0, len(serviceEntries))

	for _, entry := range serviceEntries {
		if filterByTags(append([]string{}, entry.Service.Tags...)) { // we take a copy here to not mutate the original slice
			filteredEntries = append(filteredEntries, entry)
		}
	}

	return filteredEntries, nil, nil
}

func consulServiceEntry(name string, tags []string, meta map[string]string) *consul.ServiceEntry {
	return &consul.ServiceEntry{
		Node: &consul.Node{
			Node: name,
		},
		Service: &consul.AgentService{
			Meta: meta,
			Tags: tags,
		},
	}
}

func TestConsulDiscoverVTGates(t *testing.T) {
	tests := []struct {
		name      string
		disco     *ConsulDiscovery
		tags      []string
		entries   map[string][]*consul.ServiceEntry
		expected  []*vtadminpb.VTGate
		shouldErr bool
	}{
		{
			name: "all gates",
			disco: &ConsulDiscovery{
				cluster:       "cluster",
				vtgateService: "vtgate",
				vtgateCellTag: "cell",
				vtgatePoolTag: "pool",
			},
			tags: []string{},
			entries: map[string][]*consul.ServiceEntry{
				"vtgate": {
					consulServiceEntry("vtgate1", []string{"pool:pool1", "cell:zone1", "extra:tag"}, nil),
					consulServiceEntry("vtgate2", []string{"pool:pool1", "cell:zone2"}, nil),
					consulServiceEntry("vtgate3", []string{"pool:pool1", "cell:zone3"}, nil),
				},
			},
			expected: []*vtadminpb.VTGate{
				{
					Cluster:  "cluster",
					Hostname: "vtgate1",
					Cell:     "zone1",
					Pool:     "pool1",
				},
				{
					Cluster:  "cluster",
					Hostname: "vtgate2",
					Cell:     "zone2",
					Pool:     "pool1",
				},
				{
					Cluster:  "cluster",
					Hostname: "vtgate3",
					Cell:     "zone3",
					Pool:     "pool1",
				},
			},
			shouldErr: false,
		},
		{
			name: "one cell",
			disco: &ConsulDiscovery{
				cluster:       "cluster",
				vtgateService: "vtgate",
				vtgateCellTag: "cell",
				vtgatePoolTag: "pool",
			},
			tags: []string{"cell:zone1"},
			entries: map[string][]*consul.ServiceEntry{
				"vtgate": {
					consulServiceEntry("vtgate1", []string{"pool:pool1", "cell:zone1", "extra:tag"}, nil),
					consulServiceEntry("vtgate2", []string{"pool:pool1", "cell:zone2"}, nil),
					consulServiceEntry("vtgate3", []string{"pool:pool1", "cell:zone3"}, nil),
				},
			},
			expected: []*vtadminpb.VTGate{
				{
					Cluster:  "cluster",
					Hostname: "vtgate1",
					Cell:     "zone1",
					Pool:     "pool1",
				},
			},
			shouldErr: false,
		},
		{
			name: "keyspaces to watch",
			disco: &ConsulDiscovery{
				cluster:                   "cluster",
				vtgateService:             "vtgate",
				vtgateCellTag:             "cell",
				vtgatePoolTag:             "pool",
				vtgateKeyspacesToWatchTag: "keyspaces",
			},
			tags: []string{},
			entries: map[string][]*consul.ServiceEntry{
				"vtgate": {
					consulServiceEntry("vtgate1", []string{"pool:pool1", "cell:zone1"}, map[string]string{"keyspaces": "ks1,ks2"}),
				},
			},
			expected: []*vtadminpb.VTGate{
				{
					Cluster:   "cluster",
					Hostname:  "vtgate1",
					Cell:      "zone1",
					Pool:      "pool1",
					Keyspaces: []string{"ks1", "ks2"},
				},
			},
			shouldErr: false,
		},
		{
			name: "error",
			disco: &ConsulDiscovery{
				cluster:                   "cluster",
				vtgateService:             "vtgate",
				vtgateCellTag:             "cell",
				vtgatePoolTag:             "pool",
				vtgateKeyspacesToWatchTag: "keyspaces",
			},
			tags:      []string{},
			entries:   nil,
			expected:  []*vtadminpb.VTGate{},
			shouldErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.disco.client = &fakeConsulClient{
				health: &fakeConsulHealth{
					entries: tt.entries,
				},
			}

			gates, err := tt.disco.DiscoverVTGates(context.Background(), tt.tags)
			if tt.shouldErr {
				assert.Error(t, err, assert.AnError)
				return
			}

			assert.NoError(t, err)
			assert.Equal(t, tt.expected, gates)
		})
	}
}

func TestConsulDiscoverVTGate(t *testing.T) {
	tests := []struct {
		name      string
		disco     *ConsulDiscovery
		tags      []string
		entries   map[string][]*consul.ServiceEntry
		expected  *vtadminpb.VTGate
		shouldErr bool
	}{
		{
			name: "success",
			disco: &ConsulDiscovery{
				cluster:       "cluster",
				vtgateService: "vtgate",
				vtgateCellTag: "cell",
				vtgatePoolTag: "pool",
			},
			tags: []string{"cell:zone1"},
			entries: map[string][]*consul.ServiceEntry{
				"vtgate": {
					consulServiceEntry("vtgate1", []string{"pool:pool1", "cell:zone1"}, nil),
					consulServiceEntry("vtgate2", []string{"pool:pool1", "cell:zone2"}, nil),
					consulServiceEntry("vtgate3", []string{"pool:pool1", "cell:zone3"}, nil),
				},
			},
			expected: &vtadminpb.VTGate{
				Cluster:  "cluster",
				Hostname: "vtgate1",
				Cell:     "zone1",
				Pool:     "pool1",
			},
			shouldErr: false,
		},
		{
			name: "no gates",
			disco: &ConsulDiscovery{
				cluster:       "cluster",
				vtgateService: "vtgate",
				vtgateCellTag: "cell",
				vtgatePoolTag: "pool",
			},
			tags: []string{"cell:zone1"},
			entries: map[string][]*consul.ServiceEntry{
				"vtgate": {},
			},
			expected: &vtadminpb.VTGate{
				Cluster:  "cluster",
				Hostname: "vtgate1",
				Cell:     "zone1",
				Pool:     "pool1",
			},
			shouldErr: true,
		},
		{
			name: "error",
			disco: &ConsulDiscovery{
				cluster:       "cluster",
				vtgateService: "vtgate",
				vtgateCellTag: "cell",
				vtgatePoolTag: "pool",
			},
			tags:      []string{"cell:zone1"},
			entries:   nil,
			expected:  nil,
			shouldErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.disco.client = &fakeConsulClient{
				health: &fakeConsulHealth{
					entries: tt.entries,
				},
			}

			gate, err := tt.disco.DiscoverVTGate(context.Background(), tt.tags)
			if tt.shouldErr {
				assert.Error(t, err, assert.AnError)
				return
			}

			assert.NoError(t, err)
			assert.Equal(t, tt.expected, gate)
		})
	}
}

func TestConsulDiscoverVTGateAddr(t *testing.T) {
	tests := []struct {
		name      string
		disco     *ConsulDiscovery
		tags      []string
		entries   map[string][]*consul.ServiceEntry
		expected  string
		shouldErr bool
	}{
		{
			name: "default template",
			disco: &ConsulDiscovery{
				cluster:        "cluster",
				vtgateService:  "vtgate",
				vtgateCellTag:  "cell",
				vtgatePoolTag:  "pool",
				vtgateAddrTmpl: template.Must(template.New("").Parse("{{ .Hostname }}")),
			},
			tags: []string{},
			entries: map[string][]*consul.ServiceEntry{
				"vtgate": {
					consulServiceEntry("vtgate1", []string{"pool:pool1", "cell:zone1"}, nil),
				},
			},
			expected:  "vtgate1",
			shouldErr: false,
		},
		{
			name: "custom template",
			disco: &ConsulDiscovery{
				cluster:        "cluster",
				vtgateService:  "vtgate",
				vtgateCellTag:  "cell",
				vtgatePoolTag:  "pool",
				vtgateAddrTmpl: template.Must(template.New("").Parse("{{ .Cluster }}-{{ .Pool }}-{{ .Cell }}-{{ .Hostname }}.example.com:15000")), // nolint:lll
			},
			tags: []string{},
			entries: map[string][]*consul.ServiceEntry{
				"vtgate": {
					consulServiceEntry("vtgate1", []string{"pool:pool1", "cell:zone1"}, nil),
				},
			},
			expected:  "cluster-pool1-zone1-vtgate1.example.com:15000",
			shouldErr: false,
		},
		{
			name: "error",
			disco: &ConsulDiscovery{
				cluster:        "cluster",
				vtgateService:  "vtgate",
				vtgateCellTag:  "cell",
				vtgatePoolTag:  "pool",
				vtgateAddrTmpl: template.Must(template.New("").Parse("{{ .Hostname }}")),
			},
			tags:      []string{},
			entries:   nil,
			expected:  "",
			shouldErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.disco.client = &fakeConsulClient{
				health: &fakeConsulHealth{
					entries: tt.entries,
				},
			}

			addr, err := tt.disco.DiscoverVTGateAddr(context.Background(), tt.tags)
			if tt.shouldErr {
				assert.Error(t, err, assert.AnError)
				return
			}

			assert.NoError(t, err)
			assert.Equal(t, tt.expected, addr)
		})
	}
}
