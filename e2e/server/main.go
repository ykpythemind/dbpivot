package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
)

type item struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

func main() {
	dsn := os.Getenv("DSN")
	if dsn == "" {
		dsn = "postgres://anyone:any@127.0.0.1:6432/appdb?sslmode=disable"
	}
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = "127.0.0.1:18080"
	}

	http.HandleFunc("/items", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			http.Error(w, fmt.Sprintf("connect: %v", err), http.StatusBadGateway)
			return
		}
		defer conn.Close(ctx)

		rows, err := conn.Query(ctx, "SELECT id, name FROM items ORDER BY id")
		if err != nil {
			http.Error(w, fmt.Sprintf("query: %v", err), http.StatusBadGateway)
			return
		}
		defer rows.Close()

		out := []item{}
		for rows.Next() {
			var it item
			if err := rows.Scan(&it.ID, &it.Name); err != nil {
				http.Error(w, fmt.Sprintf("scan: %v", err), http.StatusInternalServerError)
				return
			}
			out = append(out, it)
		}
		if err := rows.Err(); err != nil {
			http.Error(w, fmt.Sprintf("rows: %v", err), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})

	log.Printf("e2e server listening on http://%s (DSN=%s)", addr, dsn)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal(err)
	}
}
