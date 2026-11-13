package handlebars_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	handlebars "github.com/nikthespirit/go-handlebars"
)

func Example() {
	tpl, err := handlebars.Parse(`<h1>{{title}}</h1>
<ul>
{{#each items}}
  {{> item}}
{{/each}}
</ul>
`)
	if err != nil {
		panic(err)
	}
	if err := tpl.RegisterPartial("item", "<li>{{shout name}}{{#if @last}} (last){{/if}}</li>\n"); err != nil {
		panic(err)
	}
	tpl.RegisterHelper("shout", func(o *handlebars.HelperOptions) (any, error) {
		return strings.ToUpper(fmt.Sprint(o.Args[0])) + "!", nil
	})

	out, err := tpl.Render(map[string]any{
		"title": "Fish & chips",
		"items": []any{
			map[string]any{"name": "cod"},
			map[string]any{"name": "haddock"},
		},
	})
	if err != nil {
		panic(err)
	}
	fmt.Print(out)
	// Output:
	// <h1>Fish &amp; chips</h1>
	// <ul>
	//   <li>COD!</li>
	//   <li>HADDOCK! (last)</li>
	// </ul>
}

// Templates from untrusted sources: tighten the limits you care about
// (zero fields keep their DefaultLimits value) and render under a deadline
// as well. Here the output limit stops the render long before the deadline.
func Example_untrusted() {
	opts := handlebars.Options{Limits: handlebars.Limits{MaxOutputSize: 1 << 10}}
	tpl, err := handlebars.ParseWithOptions("{{#each rows}}{{#each @root.rows}}{{.}}{{/each}}{{/each}}", opts)
	if err != nil {
		panic(err)
	}
	rows := make([]any, 100)
	for i := range rows {
		rows[i] = "0123456789"
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = tpl.RenderCtx(ctx, io.Discard, map[string]any{"rows": rows})

	var le *handlebars.LimitError
	if errors.As(err, &le) {
		fmt.Println(le.What, "limit exceeded, max", le.Max)
	}
	// Output:
	// output size limit exceeded, max 1024
}
