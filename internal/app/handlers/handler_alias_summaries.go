package handlers

import (
	"context"
	"sort"

	"github.com/thushan/olla/internal/adapter/converter"
	"github.com/thushan/olla/internal/core/domain"
)

// endpointMaps returns the healthy-endpoint URL set and the URL-to-name map
// from a single repository pass. GetHealthy is only a Status filter of
// GetAll's slice, so calling both was two RLocks plus two full-slice copies
// per models request. A nil healthy map means the repository failed; callers
// bail out on it.
func (a *Application) endpointMaps(ctx context.Context) (healthy map[string]bool, names map[string]string) {
	endpoints, err := a.repository.GetAll(ctx)
	if err != nil {
		return nil, nil
	}
	healthy = make(map[string]bool, len(endpoints))
	names = make(map[string]string, len(endpoints))
	for _, ep := range endpoints {
		names[ep.URLString] = ep.Name
		if ep.Status == domain.StatusHealthy {
			healthy[ep.URLString] = true
		}
	}
	return healthy, names
}

// liveModelIndex indexes unified models by native name / canonical ID, keeping
// only those with at least one healthy source endpoint.
type liveModelIndex struct {
	caps []string
	eps  []string
}

func buildLiveModelIndex(unifiedModels []*domain.UnifiedModel, healthyMap map[string]bool) map[string]*liveModelIndex {
	index := make(map[string]*liveModelIndex, len(unifiedModels))
	for _, um := range unifiedModels {
		var liveEPs []string
		for _, ep := range um.SourceEndpoints {
			if healthyMap[ep.EndpointURL] {
				liveEPs = append(liveEPs, ep.EndpointURL)
			}
		}
		if len(liveEPs) == 0 {
			continue
		}
		entry := &liveModelIndex{caps: um.Capabilities, eps: liveEPs}
		index[um.ID] = entry
		for _, alias := range um.Aliases {
			if alias.Name != "" {
				if _, exists := index[alias.Name]; !exists {
					index[alias.Name] = entry
				}
			}
		}
	}
	return index
}

// aliasUnion computes the union of capabilities across live carriers for
// one alias, returning the sorted capability list and the set of live endpoint URLs.
func aliasUnion(carriers []string, index map[string]*liveModelIndex) (caps []string, eps []string) {
	capSet := make(map[string]struct{})
	for _, carrier := range carriers {
		mi, ok := index[carrier]
		if !ok {
			continue
		}
		for _, c := range mi.caps {
			capSet[c] = struct{}{}
		}
		eps = append(eps, mi.eps...)
	}
	caps = make([]string, 0, len(capSet))
	for c := range capSet {
		caps = append(caps, c)
	}
	sort.Strings(caps)
	return caps, eps
}

// appendAliasesToModelResponse injects virtual alias entries into the
// converter response after conversion, preserving the response type.
func (a *Application) appendAliasesToModelResponse(ctx context.Context, response interface{}, unifiedModels []*domain.UnifiedModel) interface{} {
	if a.aliasResolver == nil {
		return response
	}
	healthyMap, epNames := a.endpointMaps(ctx)
	if healthyMap == nil {
		return response
	}

	index := buildLiveModelIndex(unifiedModels, healthyMap)

	switch resp := response.(type) {
	case converter.UnifiedModelResponse:
		resp.Data = append(resp.Data, a.aliasUnifiedData(index, epNames)...)
		return resp
	case converter.OpenAIModelResponse:
		resp.Data = append(resp.Data, a.aliasOpenAIData()...)
		return resp
	}
	return response
}

func (a *Application) aliasUnifiedData(index map[string]*liveModelIndex, epNames map[string]string) []converter.UnifiedModelData {
	aliasNames := a.aliasResolver.Names()
	out := make([]converter.UnifiedModelData, 0, len(aliasNames))
	for _, name := range aliasNames {
		carriers := a.aliasResolver.GetActualModels(name)
		caps, eps := aliasUnion(carriers, index)
		names := resolveEndpointNames(eps, epNames)
		out = append(out, converter.UnifiedModelData{
			ID:      name,
			Object:  "model",
			OwnedBy: "olla",
			Olla: &converter.OllaExtensions{
				Aliases:      carriers,
				Capabilities: caps,
				Availability: epsToAvailability(names),
			},
		})
	}
	return out
}

func (a *Application) aliasOpenAIData() []converter.OpenAIModelData {
	aliasNames := a.aliasResolver.Names()
	out := make([]converter.OpenAIModelData, 0, len(aliasNames))
	for _, name := range aliasNames {
		out = append(out, converter.OpenAIModelData{
			ID:      name,
			Object:  "model",
			OwnedBy: "olla",
		})
	}
	return out
}

func resolveEndpointNames(urls []string, epNames map[string]string) []string {
	seen := make(map[string]struct{}, len(urls))
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		name := epNames[u]
		if name == "" {
			name = u
		}
		if _, ok := seen[name]; !ok {
			seen[name] = struct{}{}
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func epsToAvailability(names []string) []converter.EndpointStatus {
	out := make([]converter.EndpointStatus, 0, len(names))
	for _, n := range names {
		out = append(out, converter.EndpointStatus{Endpoint: n, State: "alias"})
	}
	return out
}

// appendAliasModelSummaries injects alias virtual-model entries into the
// /internal/status/models listing. Capabilities = union of live carriers.
func (a *Application) appendAliasModelSummaries(ctx context.Context, allModels []ModelSummary) []ModelSummary {
	if a.aliasResolver == nil {
		return allModels
	}

	getter, ok := a.modelRegistry.(unifiedModelsGetter)
	if !ok {
		return allModels
	}
	unified, err := getter.GetUnifiedModels(ctx)
	if err != nil || len(unified) == 0 {
		return allModels
	}
	healthyMap, epNames := a.endpointMaps(ctx)
	if healthyMap == nil {
		return allModels
	}

	index := buildLiveModelIndex(unified, healthyMap)

	for _, name := range a.aliasResolver.Names() {
		carriers := a.aliasResolver.GetActualModels(name)
		caps, eps := aliasUnion(carriers, index)
		epList := resolveEndpointNames(eps, epNames)

		allModels = append(allModels, ModelSummary{
			Name:         name,
			Type:         "alias",
			Endpoints:    epList,
			Aliases:      carriers,
			Capabilities: caps,
		})
	}
	return allModels
}
