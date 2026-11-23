package rpc

import (
	"errors"
	"fmt"
	"slices"
)

func (s *Server) handleSteerPrompt(req Request) {
	if req.TurnID == "" || req.ActiveTurnID == "" {
		s.respond(req, nil, errors.New("steer_prompt needs turn_id and active_turn_id"))
		return
	}
	if err := s.scheduleSteering(req.TurnID, req.ActiveTurnID); err != nil {
		s.respond(req, nil, err)
		return
	}
	s.respond(req, map[string]any{"turn_id": req.TurnID, "active_turn_id": req.ActiveTurnID}, nil)
}

func (s *Server) scheduleSteering(queued, active string) error {
	s.workMu.Lock()
	defer s.workMu.Unlock()
	if s.steeringTurn != active {
		return fmt.Errorf("turn %q is no longer accepting steering", active)
	}
	if !slices.ContainsFunc(s.work, matchesPrompt(queued)) {
		return fmt.Errorf("turn %q is not queued", queued)
	}
	if s.steeringClaim != queued && !slices.Contains(s.steeringQueue, queued) {
		s.steeringQueue = append(s.steeringQueue, queued)
	}
	return nil
}

func matchesPrompt(turnID string) func(workItem) bool {
	return func(item workItem) bool {
		return item.kind == workPrompt && item.req.TurnID == turnID
	}
}

// receiveSteering is called by the agent goroutine. Keep each prompt's place
// in the queue until its message is saved, so a turn that ends early or a
// failed save leaves the remaining prompts available for standalone turns.
func (s *Server) receiveSteering(finishing bool, deliver func(string) bool) {
	for range maxQueue {
		s.workMu.Lock()
		if len(s.steeringQueue) == 0 {
			if finishing {
				s.steeringTurn = ""
			}
			s.workMu.Unlock()
			return
		}
		turnID := s.steeringQueue[0]
		s.steeringQueue = s.steeringQueue[1:]
		idx := slices.IndexFunc(s.work, matchesPrompt(turnID))
		req := s.work[idx].req
		s.steeringClaim = turnID
		s.workMu.Unlock()

		saved := deliver(req.Message)
		s.workMu.Lock()
		if saved {
			s.work = slices.DeleteFunc(s.work, matchesPrompt(turnID))
		}
		s.steeringClaim = ""
		s.workMu.Unlock()
		if !saved {
			return
		}
		s.writeEvent(Event{Type: "prompt_steered", OriginTurnID: turnID, OriginRequestID: req.ID})
		finishing = false
	}
}

func (s *Server) closeSteering() {
	s.workMu.Lock()
	s.steeringTurn = ""
	s.steeringQueue = nil
	s.workMu.Unlock()
}
