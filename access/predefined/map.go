package predefined

import "github.com/on-keyday/kscale/access"

var resourceMap map[string]access.ResourceTemplate

func init() {
	resourceMap = make(map[string]access.ResourceTemplate)
	for _, r := range Resources() {
		resourceMap[r.Name()] = r
	}
}

func ResourceMap() map[string]access.ResourceTemplate {
	return resourceMap
}
