package documents

import "net/http"

type Service struct{}

func NewService() *Service { return &Service{} }

func (s *Service) HandleUpload(w http.ResponseWriter, r *http.Request)        {}
func (s *Service) HandleList(w http.ResponseWriter, r *http.Request)          {}
func (s *Service) HandleGet(w http.ResponseWriter, r *http.Request)           {}
func (s *Service) HandlePresignedView(w http.ResponseWriter, r *http.Request) {}
