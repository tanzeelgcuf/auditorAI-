package billing

import "net/http"

type Service struct{}

func NewService() *Service { return &Service{} }

func (s *Service) HandleCheckout(w http.ResponseWriter, r *http.Request)      {}
func (s *Service) HandleStripeWebhook(w http.ResponseWriter, r *http.Request) {}

