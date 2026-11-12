// Package benchdata holds the benchmark scenarios, shared by the library's
// benchmarks and the comparison module in bench/, so every engine renders
// identical inputs. Data is plain Go (maps, slices, strings, ints,
// float64s) that any Handlebars engine accepts; arrays only, so no engine's
// map iteration order matters.
package benchdata

import (
	"strconv"
	"strings"
)

// Scenario is one benchmark input.
type Scenario struct {
	Name     string
	Template string
	Partials map[string]string // nil when the scenario has none
	Data     func() any        // fresh data for one benchmark run
}

// Scenarios are (a) static-heavy, (b) each over 1000 items × 3 fields,
// (c) partial-heavy and (d) escape-heavy.
var Scenarios = []Scenario{
	{Name: "static", Template: staticTemplate, Data: staticData},
	{Name: "each", Template: `<ul>{{#each items}}<li id="{{id}}">{{name}}: {{price}}</li>{{/each}}</ul>`, Data: eachData},
	{
		Name:     "partials",
		Template: "<div class=\"users\">\n{{#each users}}\n  {{> card}}\n{{/each}}\n</div>\n",
		Partials: map[string]string{
			"card":   "<div class=\"card\">{{> avatar}}<h2>{{name}}</h2>{{> badge}}</div>\n",
			"avatar": `<img src="/u/{{id}}.png" alt="{{name}}">`,
			"badge":  `<span class="badge">{{role}}</span>`,
		},
		Data: partialsData,
	},
	{Name: "escape", Template: `{{#each snippets}}<pre title="{{title}}">{{code}}</pre>{{/each}}`, Data: escapeData},
}

// staticTemplate is about 6 KiB of markup with two substitutions.
var staticTemplate = `<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8"><title>{{title}}</title>
<link rel="stylesheet" href="/static/site.css"></head>
<body><header><nav><a href="/">Home</a> <a href="/docs">Docs</a> <a href="/blog">Blog</a></nav></header>
<main>
` + strings.Repeat(`<section><h2>Section heading</h2><p>Lorem ipsum dolor sit amet, consectetur adipiscing elit,
sed do eiusmod tempor incididunt ut labore et dolore magna aliqua. Ut enim ad minim veniam, quis
nostrud exercitation ullamco laboris nisi ut aliquip ex ea commodo consequat.</p></section>
`, 20) + `</main>
<footer>{{footer}}</footer></body></html>
`

func staticData() any {
	return map[string]any{"title": "Static page", "footer": "Copyright 2026"}
}

func eachData() any {
	items := make([]any, 1000)
	for i := range items {
		items[i] = map[string]any{"id": i, "name": "item " + strconv.Itoa(i), "price": float64(i) + 0.25}
	}
	return map[string]any{"items": items}
}

func partialsData() any {
	users := make([]any, 100)
	for i := range users {
		role := "member"
		if i%10 == 0 {
			role = "admin"
		}
		users[i] = map[string]any{"id": i, "name": "User " + strconv.Itoa(i), "role": role}
	}
	return map[string]any{"users": users}
}

func escapeData() any {
	code := strings.Repeat(`<a href="/search?q=a&b=c" onclick='go("x")'>it's a `+"`tag`"+` = <b>&amp;</b></a>`+"\n", 16)
	snippets := make([]any, 20)
	for i := range snippets {
		snippets[i] = map[string]any{"title": `"snippet" <` + strconv.Itoa(i) + `>`, "code": code}
	}
	return map[string]any{"snippets": snippets}
}
