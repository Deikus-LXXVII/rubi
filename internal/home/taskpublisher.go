//go:build !rubi_acceptance

package home

import "github.com/Deikus-LXXVII/rubi/internal/agenttask"

func taskPublisher(store agenttask.Store, helper string) agenttask.Publisher {
	return agenttask.Publisher{Store: store, Helper: helper}
}
