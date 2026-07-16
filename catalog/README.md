# Company catalog audit

`morethanfaangm-audit.tsv` accounts for every list row parsed from
[Kaustubh-Natuskar/moreThanFAANGM](https://github.com/Kaustubh-Natuskar/moreThanFAANGM)
at commit `a91b6120e47091bd1b987a566689a3f58f5252cb`.

The source README advertises 481 companies but contains 483 links: 481 unique
company names, one exact Ninjacart duplicate, and one maintainer/contact link.
Each row was checked on 2026-07-16 by resolving the supplied URL, identifying
the current ATS from first-party evidence, and probing any supported public API.

The final audit contains 137 validated rows (139 distinct boards because Nike
and Visa each expose two), 9 duplicates, 261 unsupported systems, 36 dead
links/boards, 39 manual-review cases, and 1 non-company maintainer link. The
139 validated identities are present in `config.example.yaml`; combined with
the original catalog, Jobwatch now polls 184 unique ATS boards.

Dispositions mean:

- `validated_supported`: first-party identity evidence and the exact public ATS API both succeeded.
- `duplicate`: the row repeats another source row or a canonical ATS board already configured in Jobwatch.
- `unsupported`: the company currently uses an ATS or careers system for which Jobwatch has no adapter.
- `dead`: the supplied/current board could not be used and no live supported replacement was verified.
- `manual_review`: evidence was ambiguous, incomplete, regional-only, group-wide, or had no usable live postings.
- `not_a_company`: the parsed row is repository metadata rather than a company.

The checked job counts are point-in-time evidence, not expected constants.
They are deliberately not used by runtime code or offline tests.
