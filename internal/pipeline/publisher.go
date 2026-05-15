package pipeline

import (
	"log"

	"recipe-bot/internal/storage"
	"recipe-bot/internal/telegram"
)

// Publisher takes the oldest "ready" recipe from the queue and posts it to the
// channel. It runs on a schedule (twice daily by default). Keeping fetch and
// publish separate means a publish never waits on a slow external API call.
type Publisher struct {
	store *storage.Storage
	tg    *telegram.Client
}

// NewPublisher wires the publisher's dependencies.
func NewPublisher(store *storage.Storage, tg *telegram.Client) *Publisher {
	return &Publisher{store: store, tg: tg}
}

// Run publishes exactly one recipe. If the queue is empty it logs a warning so
// the operator knows the fetcher needs attention.
func (p *Publisher) Run() {
	rec, err := p.store.NextReady()
	if err != nil {
		log.Printf("[publisher] next ready: %v", err)
		return
	}
	if rec == nil {
		log.Printf("[publisher] queue empty — nothing to publish (check the fetcher)")
		return
	}

	log.Printf("[publisher] publishing %d %q", rec.ID, rec.Title)

	var publishErr error
	if rec.ImagePath != "" {
		publishErr = p.tg.Publish(rec.ImagePath, rec.Content)
	} else {
		publishErr = p.tg.PublishText(rec.Content)
	}
	if publishErr != nil {
		log.Printf("[publisher] failed to publish %d: %v", rec.ID, publishErr)
		if markErr := p.store.MarkFailed(rec.ID, publishErr.Error()); markErr != nil {
			log.Printf("[publisher] mark failed %d: %v", rec.ID, markErr)
		}
		return
	}

	if err := p.store.MarkPublished(rec.ID); err != nil {
		log.Printf("[publisher] mark published %d: %v", rec.ID, err)
		return
	}
	if n, err := p.store.CountReady(); err == nil {
		log.Printf("[publisher] published %d — %d recipes still ready", rec.ID, n)
	} else {
		log.Printf("[publisher] published %d", rec.ID)
	}
}
