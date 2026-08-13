package runtimeview

import (
	"context"
	"sort"
)

type eventObject struct {
	kind string
	name string
}

func (s *Service) Events(ctx context.Context, request EventRequest) (EventSnapshot, error) {
	if err := s.ensureSecure(); err != nil {
		return EventSnapshot{}, err
	}
	if request.Limit == 0 {
		request.Limit = min(s.config.MaxEvents, 50)
	}
	if request.Limit < 1 || request.Limit > s.config.MaxEvents {
		return EventSnapshot{}, ErrInvalidRequest
	}
	target, err := s.resolve(ctx, request.Target)
	if err != nil {
		return EventSnapshot{}, err
	}
	graph, err := s.discoverGraph(ctx, target)
	if err != nil {
		return EventSnapshot{}, err
	}
	objects := make(map[string]eventObject, len(graph.deployments)+len(graph.statefulSets)+len(graph.replicaSets)+len(graph.pods))
	for _, deployment := range graph.deployments {
		objects[deployment.resource.UID] = eventObject{kind: "Deployment", name: deployment.resource.Name}
	}
	for _, replicaSet := range graph.replicaSets {
		objects[replicaSet.resource.UID] = eventObject{kind: "ReplicaSet", name: replicaSet.resource.Name}
	}
	for _, statefulSet := range graph.statefulSets {
		objects[statefulSet.resource.UID] = eventObject{kind: "StatefulSet", name: statefulSet.resource.Name}
	}
	for _, pod := range graph.pods {
		objects[pod.resource.UID] = eventObject{kind: "Pod", name: pod.resource.Name}
	}
	uids := make([]string, 0, len(objects))
	for uid := range objects {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	events, err := s.client.ListEvents(ctx, target.Namespace, EventQuery{InvolvedUIDs: uids, Limit: request.Limit + 1})
	if err != nil {
		return EventSnapshot{}, err
	}
	items := make([]RuntimeEvent, 0, min(len(events), request.Limit))
	seen := make(map[string]struct{}, len(events))
	for _, event := range events {
		if event.Namespace != target.Namespace {
			return EventSnapshot{}, ErrScopeViolation
		}
		object, allowed := objects[event.InvolvedUID]
		if !allowed {
			return EventSnapshot{}, ErrScopeViolation
		}
		if !uidPattern.MatchString(event.UID) {
			return EventSnapshot{}, ErrScopeViolation
		}
		if _, duplicate := seen[event.UID]; duplicate {
			continue
		}
		seen[event.UID] = struct{}{}
		message := sanitizeText(event.Message)
		message = sanitizeText(s.redactor.Redact(message))
		messageTruncated := len(message) > s.config.MaxEventMessageBytes
		if messageTruncated {
			message = truncateUTF8(message, s.config.MaxEventMessageBytes)
		}
		count := event.Count
		if count < 1 {
			count = 1
		}
		items = append(items, RuntimeEvent{
			ID:               event.UID,
			Type:             eventType(event.Type),
			Reason:           safeReason(event.Reason),
			Message:          message,
			MessageTruncated: messageTruncated,
			ObjectKind:       object.kind,
			ObjectName:       object.name,
			Count:            count,
			FirstSeen:        event.FirstSeen.UTC(),
			LastSeen:         event.LastSeen.UTC(),
		})
	}
	sort.Slice(items, func(i, j int) bool {
		if !items[i].LastSeen.Equal(items[j].LastSeen) {
			return items[i].LastSeen.After(items[j].LastSeen)
		}
		return items[i].ID < items[j].ID
	})
	truncated := len(items) > request.Limit
	if truncated {
		items = items[:request.Limit]
	}
	return EventSnapshot{Items: items, Truncated: truncated, ObservedAt: s.now().UTC()}, nil
}
