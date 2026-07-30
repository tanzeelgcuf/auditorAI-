package findings

import "net/http"

type Service struct{}

func NewService() *Service { return &Service{} }

func (s *Service) HandleList(w http.ResponseWriter, r *http.Request)           {}
func (s *Service) HandleAddComment(w http.ResponseWriter, r *http.Request)     {}
func (s *Service) HandleUpdateStatus(w http.ResponseWriter, r *http.Request)   {}
func (s *Service) HandleAddAttachment(w http.ResponseWriter, r *http.Request)  {}
func (s *Service) HandleGenerateReport(w http.ResponseWriter, r *http.Request) {}
func (s *Service) HandleGetReport(w http.ResponseWriter, r *http.Request)      {}
func (s *Service) HandleGetCitation(w http.ResponseWriter, r *http.Request)    {}
