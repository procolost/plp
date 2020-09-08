package allocator

import (
	"errors"
	"fmt"
	"net"

	"purelb.io/pkg/apis/v1"
)

// An Allocator tracks IP address pools and allocates addresses from them.
type Allocator struct {
	pools     map[string]Pool
	allocated map[string]*alloc // svc -> alloc
}

type alloc struct {
	pool  string
	ip    net.IP
	ports []Port
	Key
}

// New returns an Allocator managing no pools.
func New() *Allocator {
	return &Allocator{
		pools:     map[string]Pool{},
		allocated: map[string]*alloc{},
	}
}

// SetPools updates the set of address pools that the allocator owns.
func (a *Allocator) SetPools(groups []*v1.ServiceGroup) error {
	pools, err := parseConfig(groups)
	if err != nil {
		return err
	}

	for n := range a.pools {
		if pools[n] == nil {
			stats.poolCapacity.DeleteLabelValues(n)
			stats.poolActive.DeleteLabelValues(n)
			stats.poolAllocated.DeleteLabelValues(n)
		}
	}

	a.pools = pools

	// Refresh or initiate stats
	for n, p := range a.pools {
		stats.poolCapacity.WithLabelValues(n).Set(float64(p.Size()))
		stats.poolActive.WithLabelValues(n).Set(float64(p.InUse()))
	}

	return nil
}

// assign unconditionally updates internal state to reflect svc's
// allocation of alloc. Caller must ensure that this call is safe.
func (a *Allocator) assign(svc string, alloc *alloc) {
	a.Unassign(svc)
	a.allocated[svc] = alloc

	pool := a.pools[alloc.pool]
	pool.Assign(alloc.ip, alloc.ports, svc, &alloc.Key)

	stats.poolCapacity.WithLabelValues(alloc.pool).Set(float64(a.pools[alloc.pool].Size()))
	stats.poolActive.WithLabelValues(alloc.pool).Set(float64(pool.InUse()))
}

// Assign assigns the requested ip to svc, if the assignment is
// permissible by sharingKey and backendKey.
func (a *Allocator) Assign(svc string, ip net.IP, ports []Port, sharingKey, backendKey string) (string, error) {
	pool := poolFor(a.pools, ip)
	if pool == "" {
		return "", fmt.Errorf("%q is not allowed in config", ip)
	}
	sk := &Key{
		Sharing: sharingKey,
		Backend: backendKey,
	}

	// Does the IP already have allocs? If so, needs to be the same
	// sharing key, and have non-overlapping ports. If not, the proposed
	// IP needs to be allowed by configuration.
	err := a.pools[pool].Available(ip, ports, svc, sk)	// FIXME: this should Assign() here, not check Available.  Might need to iterate over pools rather than do poolFor
	if err != nil {
		return "", err
	}

	// Either the IP is entirely unused, or the requested use is
	// compatible with existing uses. Assign!
	alloc := &alloc{
		pool:  pool,
		ip:    ip,
		ports: make([]Port, len(ports)),
		Key:   *sk,
	}
	for i, port := range ports {
		alloc.ports[i] = port
	}
	a.assign(svc, alloc)
	return pool, nil
}

// Unassign frees the IP associated with service, if any.
func (a *Allocator) Unassign(svc string) bool {
	if a.allocated[svc] == nil {
		return false
	}

	al := a.allocated[svc]

	// tell the pool that the address has been released. there might not
	// be a pool, e.g., in the case of a config change that move
	// addresses from one pool to another
	pool, tracked := a.pools[al.pool]
	if tracked {
		pool.Release(al.ip, svc)
		stats.poolActive.WithLabelValues(al.pool).Set(float64(pool.InUse()))
	}

	delete(a.allocated, svc)

	return true
}

func ipIsIPv6(ip net.IP) bool {
	return ip.To4() == nil
}

// AllocateFromPool assigns an available IP from pool to service.
func (a *Allocator) AllocateFromPool(svc string, isIPv6 bool, poolName string, ports []Port, sharingKey, backendKey string) (net.IP, error) {
	var ip net.IP

	if alloc := a.allocated[svc]; alloc != nil {
		// Handle the case where the svc has already been assigned an IP but from the wrong family.
		// This "should-not-happen" since the "ipFamily" is an immutable field in services.
		if isIPv6 != ipIsIPv6(alloc.ip) {
			return nil, fmt.Errorf("IP for wrong family assigned %s", alloc.ip.String())
		}
		if _, err := a.Assign(svc, alloc.ip, ports, sharingKey, backendKey); err != nil {
			return nil, err
		}
		return alloc.ip, nil
	}

	pool := a.pools[poolName]
	if pool == nil {
		return nil, fmt.Errorf("unknown pool %q", poolName)
	}
	if pool.IsIPV6() != isIPv6 {
		return nil, fmt.Errorf("pool %q is the wrong IP family", poolName)
	}

	sk := &Key{
		Sharing: sharingKey,
		Backend: backendKey,
	}
	ip, err := pool.AssignNext(svc, ports, sk)
	if err != nil {
		// Woops, no IPs :( Fail.
		return nil, err
	}

	alloc := &alloc{
		pool:  poolName,
		ip:    ip,
		ports: make([]Port, len(ports)),
		Key:   *sk,
	}
	for i, port := range ports {
		alloc.ports[i] = port
	}
	a.assign(svc, alloc)

	return ip, nil
}

// Allocate assigns any available and assignable IP to service.
func (a *Allocator) Allocate(svc string, isIPv6 bool, ports []Port, sharingKey, backendKey string) (string, net.IP, error) {
	if alloc := a.allocated[svc]; alloc != nil {
		if _, err := a.Assign(svc, alloc.ip, ports, sharingKey, backendKey); err != nil {
			return "", nil, err
		}
		return alloc.pool, alloc.ip, nil
	}

	for poolName := range a.pools {
		if !a.pools[poolName].AutoAssign() {
			continue
		}
		if ip, err := a.AllocateFromPool(svc, isIPv6, poolName, ports, sharingKey, backendKey); err == nil {
			return poolName, ip, nil
		}
	}

	return "", nil, errors.New("no available IPs")
}

// IP returns the IP address allocated to service, or nil if none are allocated.
func (a *Allocator) IP(svc string) net.IP {
	if alloc := a.allocated[svc]; alloc != nil {
		return alloc.ip
	}
	return nil
}

// Pool returns the pool from which service's IP was allocated. If
// service has no IP allocated, "" is returned.
func (a *Allocator) Pool(svc string) string {
	ip := a.IP(svc)
	if ip == nil {
		return ""
	}
	return poolFor(a.pools, ip)
}

// poolFor returns the pool that owns the requested IP, or "" if none.
func poolFor(pools map[string]Pool, ip net.IP) string {
	for pname, p := range pools {
		if p.Contains(ip) {
			return pname
		}
	}
	return ""
}

func parseConfig(groups []*v1.ServiceGroup) (map[string]Pool, error) {
	pools := map[string]Pool{}

	for i, group := range groups {
		pool, err := parseGroup(group.Name, group.Spec)
		if err != nil {
			return nil, fmt.Errorf("parsing address pool #%d: %s", i+1, err)
		}

		// Check that the pool isn't already defined
		if pools[group.Name] != nil {
			return nil, fmt.Errorf("duplicate definition of pool %q", group.Name)
		}

		// Check that this pool doesn't overlap with any of the previous
		// ones
		for name, r := range pools {
			if pool.Overlaps(r) {
				return nil, fmt.Errorf("pool %q overlaps with already defined pool %q", group.Name, name)
			}
		}

		pools[group.Name] = pool
	}

	return pools, nil
}

func parseGroup(name string, group v1.ServiceGroupSpec) (Pool, error) {
	if group.Local != nil {
		ret, err := NewLocalPool(group.AutoAssign, group.Local.Pool, group.Local.Subnet, group.Local.Aggregation)
		if err != nil {
			return nil, err
		}
		return *ret, nil
	} else if group.EGW != nil {
		ret, err := NewEGWPool(group.AutoAssign, group.EGW.URL, group.EGW.Aggregation)
		if err != nil {
			return nil, err
		}
		return *ret, nil
	}

	return nil, fmt.Errorf("Pool is not local or EGW")
}
