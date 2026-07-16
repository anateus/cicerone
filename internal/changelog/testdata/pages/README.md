# Extractor fixture bake-off

Evaluated against `release.html` and `malformed.html`. Each fixture awards one
point for retaining a version heading, retaining list items, removing
navigation/footer boilerplate, deterministic output, and recovering useful
content from malformed HTML (10 points total).

| Extractor | Revision evaluated | Heading | Lists | Boilerplate | Deterministic | Malformed | Score |
|---|---|---:|---:|---:|---:|---:|---:|
| go-shiori/go-readability | `v0.0.0-20251205110129-5db1dc9836f0` | 2 | 2 | 2 | 2 | 2 | **10/10** |
| go-trafilatura | `v1.12.2` | 0 | 0 | 2 | 2 | 2 | **6/10** |

The published Go modules do not contain the planned `v0.3.1` and `v1.13.4`
tags (the proxy exposes no semantic tags for go-readability and go-trafilatura
through `v1.12.2`). The closest available revisions were evaluated.

Readability wins because its content tree preserves heading and list structure,
which is required for terminal plain text. The Trafilatura adapter and dependency
were removed after the comparison.
