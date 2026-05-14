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
	rec, ok := p.store.NextReady()
	if !ok {
		log.Printf("[publisher] queue empty — nothing to publish (check the fetcher)")
		return
	}

	log.Printf("[publisher] publishing %d %q", rec.ID, rec.Title)

	var err error
	if rec.ImagePath != "" {
		err = p.tg.Publish(rec.ImagePath, rec.Content)
	} else {
		err = p.tg.PublishText(rec.Content)
	}
	if err != nil {
		log.Printf("[publisher] failed to publish %d: %v", rec.ID, err)
		if markErr := p.store.MarkFailed(rec.ID); markErr != nil {
			log.Printf("[publisher] mark failed %d: %v", rec.ID, markErr)
		}
		return
	}

	if err := p.store.MarkPublished(rec.ID); err != nil {
		log.Printf("[publisher] mark published %d: %v", rec.ID, err)
		return
	}
	log.Printf("[publisher] published %d — %d recipes still ready", rec.ID, p.store.CountReady())
}
