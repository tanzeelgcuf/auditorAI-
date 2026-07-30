package entities

import "net/http"

type Service struct{}

func NewService() *Service { return &Service{} }

func (s *Service) HandleList(w http.ResponseWriter, r *http.Request) {}
