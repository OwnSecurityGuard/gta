package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"

	_ "modernc.org/sqlite"

	"gta/pkg/event"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: read_events <sqlite-path>")
		os.Exit(1)
	}
	dbPath := os.Args[1]

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	rows, err := db.Query("SELECT id, type, source, payload FROM events LIMIT 10")
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()

	for rows.Next() {
		var id, typ, source string
		var payload []byte
		if err := rows.Scan(&id, &typ, &source, &payload); err != nil {
			log.Fatal(err)
		}
		v, err := event.UnmarshalValueMsgpack(payload)
		if err != nil {
			fmt.Printf("event %s: type=%s source=%s decode_err=%v\n", id, typ, source, err)
			continue
		}
		dataJSON, _ := v.MarshalJSON()
		fmt.Printf("event %s: type=%s source=%s data=%s\n", id, typ, source, string(dataJSON))
	}
}
