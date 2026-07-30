package review

import "net/http"

type Service struct{}

func NewService() *Service { return &Service{} }

func (s *Service) HandleList(w http.ResponseWriter, r *http.Request)        {}
func (s *Service) HandleConfirm(w http.ResponseWriter, r *http.Request)     {}
func (s *Service) HandleReject(w http.ResponseWriter, r *http.Request)      {}
func (s *Service) HandleBulkConfirm(w http.ResponseWriter, r *http.Request) {}
