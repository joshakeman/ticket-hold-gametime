// Command worker runs the Temporal checkout worker. It needs a Temporal
// server (for local use: `temporal server start-dev`) and the Postgres DB.
package main

import (
	"cmp"
	"context"
	"log"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/josh/ticket-hold-gametime/internal/checkout"
	"github.com/josh/ticket-hold-gametime/internal/store"
	"github.com/josh/ticket-hold-gametime/migrations"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx := context.Background()
	dsn := cmp.Or(os.Getenv("DATABASE_URL"), "postgres://tickets:tickets@localhost:5433/tickets?sslmode=disable")
	temporalAddr := cmp.Or(os.Getenv("TEMPORAL_ADDRESS"), "localhost:7233")

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := store.Migrate(ctx, pool, migrations.FS); err != nil {
		return err
	}

	c, err := client.Dial(client.Options{HostPort: temporalAddr})
	if err != nil {
		return err
	}
	defer c.Close()

	w := worker.New(c, checkout.TaskQueue, worker.Options{})
	w.RegisterWorkflow(checkout.CheckoutWorkflow)
	// FakePayments stands in for a real provider (Stripe etc.).
	w.RegisterActivity(&checkout.Activities{Store: store.New(pool), Payments: checkout.NewFakePayments()})

	log.Printf("checkout worker on task queue %q, Temporal at %s", checkout.TaskQueue, temporalAddr)
	return w.Run(worker.InterruptCh())
}
