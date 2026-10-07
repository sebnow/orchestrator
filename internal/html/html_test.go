package html_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/sebnow/orchestrator/internal/html"
)

func render(t *testing.T, node html.Node) string {
	t.Helper()
	var out strings.Builder
	if err := html.Render(&out, node); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestGivenMarkupInTextWhenRenderedThenItIsEscaped(t *testing.T) {
	got := render(t, html.Text(`<script>alert("x") & 'y'</script>`))

	want := `&lt;script&gt;alert(&#34;x&#34;) &amp; &#39;y&#39;&lt;/script&gt;`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestGivenQuotesInAttributeValueWhenRenderedThenTheValueCannotEndEarly(t *testing.T) {
	got := render(t, html.El("a", []html.Attribute{html.Attr("title", `x" onclick="evil()`), html.Attr("href", "/a?b=1&c=<2>")}))

	want := `<a title="x&#34; onclick=&#34;evil()" href="/a?b=1&amp;c=&lt;2&gt;"></a>`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestGivenRawWhenRenderedThenItIsWrittenVerbatim(t *testing.T) {
	if got := render(t, html.Raw("<!DOCTYPE html>")); got != "<!DOCTYPE html>" {
		t.Errorf("got %s", got)
	}
}

func TestGivenVoidElementWhenRenderedThenItHasNoEndTag(t *testing.T) {
	got := render(t, html.El("input", []html.Attribute{html.Attr("name", "q"), html.Attr("required", "")}))

	if want := `<input name="q" required="">`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestGivenEmptyElementWhenRenderedThenItIsClosed(t *testing.T) {
	if got := render(t, html.El("textarea", nil)); got != "<textarea></textarea>" {
		t.Errorf("got %s", got)
	}
}

func TestGivenNestedElementsWithNilsWhenRenderedThenChildrenAreInOrderAndNilsSkipped(t *testing.T) {
	node := html.El("ul", []html.Attribute{{}, html.Attr("class", "list")},
		html.El("li", nil, html.Text("one")),
		nil,
		html.Fragment(html.El("li", nil, html.Text("two")), nil, html.El("li", nil, html.El("br", nil))),
	)

	want := `<ul class="list"><li>one</li><li>two</li><li><br></li></ul>`
	if got := render(t, node); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestGivenVoidElementWithChildrenWhenBuiltThenItPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("did not panic")
		}
	}()
	html.El("br", nil, html.Text("x"))
}

func TestGivenInvalidNameWhenBuiltThenItPanics(t *testing.T) {
	for _, name := range []string{"", "a b", "on>", `x"`, "1a", "-a", "Div"} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("%q did not panic", name)
				}
			}()
			html.Attr(name, "")
		})
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("gone") }

func TestGivenFailingWriterWhenRenderingThenTheErrorIsReturned(t *testing.T) {
	err := html.Render(failingWriter{}, html.El("p", nil, html.Text("x")))
	if err == nil || err.Error() != "gone" {
		t.Errorf("err = %v, want gone", err)
	}
}
