package services

import "sort"

// The graph is a projection of the final Compose map. Never expose environment,
// bootstrap commands or credentials through diagram metadata.
func ArchitecturePreview(compose M) M {
	services := []M{}
	all, _ := compose["services"].(M)
	keys := []string{}
	for key := range all {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		svc, ok := all[key].(M)
		if !ok {
			continue
		}
		networks := toStringList(svc["networks"])
		if names, ok := svc["networks"].(M); ok {
			for name := range names {
				networks = append(networks, name)
			}
		}
		sort.Strings(networks)
		deps := toStringList(svc["depends_on"])
		if names, ok := svc["depends_on"].(M); ok {
			for name := range names {
				deps = append(deps, name)
			}
		}
		sort.Strings(deps)
		services = append(services, M{"key": key, "image": svc["image"], "network_mode": svc["network_mode"], "networks": networks, "depends_on": deps, "ports": toStringList(svc["ports"]), "volumes": toStringList(svc["volumes"])})
	}
	return M{"version": 1, "services": services}
}
