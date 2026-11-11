package core

import "zel/internal/provider"

func newClient(p Provider) *provider.Client { return provider.New(p.BaseURL, p.APIKey, p.API) }
