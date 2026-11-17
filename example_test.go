package handlebars_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	handlebars "github.com/nikolaymatrosov/go-handlebars"
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

// Templates from untrusted sources: allow only the logic-less subset,
// tighten the limits you care about (zero fields keep their DefaultLimits
// value), check the data a template needs before rendering it, and render
// under a deadline as well. Here the output limit stops the render.
func Example_untrusted() {
	opts := handlebars.Options{
		NoBlocks: true,
		Limits:   handlebars.Limits{MaxExpressions: 100, MaxOutputSize: 1 << 10},
	}
	for _, src := range []string{"{{#each rows}}{{.}}{{/each}}", strings.Repeat("{{x}}", 101)} {
		_, err := handlebars.ParseWithOptions(src, opts)
		switch {
		case errors.Is(err, handlebars.ErrBlocksNotAllowed):
			fmt.Println("unsupported:", err)
		case errors.Is(err, handlebars.ErrExpressions):
			fmt.Println("too complex:", err)
		}
	}

	tpl, err := handlebars.ParseWithOptions("Hi {{name}}! {{repeat motto times=n}}", opts)
	if err != nil {
		panic(err)
	}
	tpl.RegisterHelper("repeat", func(o *handlebars.HelperOptions) (any, error) {
		n, _ := o.Hash["times"].(int)
		return strings.Repeat(fmt.Sprint(o.Args[0]), n), nil
	})
	fmt.Println("needs:", tpl.Variables())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = tpl.RenderCtx(ctx, io.Discard, map[string]any{"name": "Ann", "motto": "0123456789", "n": 200})

	var le *handlebars.LimitError
	if errors.Is(err, handlebars.ErrOutputSize) && errors.As(err, &le) {
		fmt.Println(le.What, "limit exceeded, max", le.Max)
	}
	// Output:
	// unsupported: Block statements are not allowed - 1:0
	// too complex: handlebars: expressions limit exceeded (max 100)
	// needs: [[name] [motto] [n]]
	// output size limit exceeded, max 1024
}
