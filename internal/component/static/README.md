# Static files

Served by the server under `/static/`.

`style.css` is written here. The two JavaScript files are vendored
unchanged from the npm registry tarballs, fetched on 2026-10-07:

| File | Package | Tarball path | SHA-256 |
| --- | --- | --- | --- |
| `htmx.min.js` | [htmx.org 2.0.11](https://registry.npmjs.org/htmx.org/-/htmx.org-2.0.11.tgz) | `package/dist/htmx.min.js` | `d6fdc75f204e6bdefa99b69bf1e6d4ac69b8a364f77929f45c13476b4000f717` |
| `htmx-ext-sse.min.js` | [htmx-ext-sse 2.2.4](https://registry.npmjs.org/htmx-ext-sse/-/htmx-ext-sse-2.2.4.tgz) | `package/dist/sse.min.js` | `98a46496de0c3605fbffdce9167ba427bdd9553184f83f149c261891a92c0136` |

The tarballs' SHA-1 sums matched the registry's `dist.shasum`. Both
packages are under the Zero-Clause BSD licence; their `LICENSE` files
are copied as `LICENSE.htmx` and `LICENSE.htmx-ext-sse`.

To update, download the new tarballs, copy the same paths over, and
update this table.
