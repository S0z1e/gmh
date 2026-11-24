package rpc

import (
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"
)

func (s *Server) handleSteerPrompt(req Request) {
	// Support single-step inline steering ({"type":"steer_prompt","message":"..."})
	// to avoid the two-step prompt -> takeWork() -> steer_prompt race window.
	if req.TurnID == "" && strings.TrimSpace(req.Message) != "" {
		accepted := make(chan struct{})
		req.accepted = accepted
		defer close(accepted)
		turnID, disposition, err := s.scheduleInlineSteering(req)
		if err != nil {
			s.respond(req, nil, err)
			return
		}
		s.respond(req, map[string]any{"turn_id": turnID, "active_turn_id": req.ActiveTurnID, "disposition": disposition}, nil)
		return
	}
	if req.TurnID == "" || req.ActiveTurnID == "" {
		s.respond(req, nil, errors.New("steer_prompt needs turn_id and active_turn_id (or message)"))
		return
	}
	if err := s.scheduleSteering(req.TurnID, req.ActiveTurnID); err != nil {
		s.respond(req, nil, err)
		return
	}
	s.respond(req, map[string]any{"turn_id": req.TurnID, "active_turn_id": req.ActiveTurnID}, nil)
}

func (s *Server) scheduleInlineSteering(req Request) (string, string, error) {
	msg := strings.TrimSpace(req.Message)
	s.workMu.Lock()
	defer s.workMu.Unlock()
	if s.closed {
		return "", "", errShutdown
	}
	if len(s.work) == maxQueue {
		return "", "", errors.New("prompt queue is full")
	}
	turnID := rand.Text()
	itemReq := Request{ID: req.ID, Type: "prompt", Message: msg, TurnID: turnID, accepted: req.accepted}
	s.work = append(s.work, workItem{kind: workPrompt, req: itemReq})
	if s.steeringTurn != "" && (req.ActiveTurnID == "" || s.steeringTurn == req.ActiveTurnID) {
		s.steeringQueue = append(s.steeringQueue, turnID)
		s.workReady.Signal()
		return turnID, "steered", nil
	}
	s.workReady.Signal()
	return turnID, "queued", nil
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
		if req.accepted != nil {
			<-req.accepted
		}

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
