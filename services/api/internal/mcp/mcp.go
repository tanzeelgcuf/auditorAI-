package mcp

import "net/http"

type Service struct{}

func NewService() *Service { return &Service{} }

func (s *Service) HandleGetPendingEntities(w http.ResponseWriter, r *http.Request) {}
func (s *Service) HandleCreateEntityLink(w http.ResponseWriter, r *http.Request)  {}
func (s *Service) HandleFlagForReview(w http.ResponseWriter, r *http.Request)      {}
func (s *Service) HandleGetBookTolerance(w http.ResponseWriter, r *http.Request)   {}
