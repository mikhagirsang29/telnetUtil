package main

import "time"

type Host struct {
	IP          string    `json:"ip"`
	Description string    `json:"description"`
	Groups      []string  `json:"groups"`
	CreatedAt   time.Time `json:"created_at"`
}

type Group struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Hosts       []string  `json:"hosts"`
	CreatedAt   time.Time `json:"created_at"`
}
