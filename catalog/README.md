# Company catalog audits

The catalog ledgers account for every physical row in two upstream company
lists. They preserve the supplied company name and career target, then record
Jobwatch's independent ATS verification and disposition:

- `morethanfaangm-audit.tsv` covers all 483 parsed links in
  [Kaustubh-Natuskar/moreThanFAANGM](https://github.com/Kaustubh-Natuskar/moreThanFAANGM)
  at commit `a91b6120e47091bd1b987a566689a3f58f5252cb`. It now has
  143 validated rows representing 145 distinct boards (Nike and Visa each
  expose two), 9 duplicates, 255 unsupported systems, 36 dead links/boards,
  39 manual-review cases, and 1 non-company maintainer link. The original
  audit was performed on 2026-07-16; later promotions carry their recheck date
  in the row evidence.
- `list-of-companies-audit.tsv` covers all 131 physical table rows in
  [nawabsahab16/List_OF_Companies](https://github.com/nawabsahab16/List_OF_Companies)
  at commit `1614c9176fe23462ec0596730c052c5d6739b637` (source README
  SHA-256 `39dc8126940e032a07dff2f4337e81c31fd98841a010ec05f6a59ca02e6abedd`).
  The 2026-07-30 audit now has 67 validated rows representing 68 distinct
  boards (MaxLinear exposes two), 35 duplicates, no unresolved unsupported
  rows, 17 dead entries, and 12 manual-review cases. Every row previously
  marked unsupported was either integrated or reclassified from current
  first-party evidence.

Five validated identities in the second ledger already occur in the legacy
ledger, so the audits represent 208 unique verified identities
(`145 + 68 - 5`). Alongside 55 other boards verified when they were added,
`config.example.yaml` now contains 263 unique job-board identities.

The List_OF_Companies repository has no license file. Its ledger therefore
retains only factual company/career-target provenance; it does not reproduce
the source table's role or compensation columns.

Dispositions mean:

- `validated_supported`: first-party identity evidence and the exact public ATS API both succeeded.
- `duplicate`: the row repeats another source row or a canonical ATS board already configured in Jobwatch.
- `unsupported`: the company currently uses an ATS or careers system for which Jobwatch has no adapter.
- `dead`: the supplied/current board could not be used and no live supported replacement was verified.
- `manual_review`: evidence was ambiguous, incomplete, regional-only, group-wide, or had no usable live postings.
- `not_a_company`: the parsed row is repository metadata rather than a company.

The checked job counts are point-in-time evidence, not expected constants.
They are deliberately not used by runtime code or offline tests.
