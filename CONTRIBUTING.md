# Contributing

Thanks for your interest in VTA Farm. This page covers the two things every contribution needs:
the licence it arrives under, and the sign-off that certifies you may submit it.

## Licence of contributions

This project is licensed under the [Apache License 2.0](LICENSE). Section 5 of that licence
provides that any contribution you intentionally submit for inclusion is licensed under the same
terms, without any additional terms or conditions. There is no contributor licence agreement (CLA)
to sign; the licence itself governs contributions.

## Developer Certificate of Origin

Every commit must carry a `Signed-off-by` trailer, which certifies that you wrote the change or
otherwise have the right to submit it under the project licence, as set out in the
[Developer Certificate of Origin](https://developercertificate.org). The trailer looks like this:

```text
Signed-off-by: Your Name <you@example.com>
```

The email address in the trailer must match the author email of the commit. Git adds the trailer
for you with `git commit -s`, and `git config --global format.signOff true` makes it automatic.
Merge commits are exempt.

A check named *DCO* runs on every pull request and fails if any commit is missing the trailer or
is signed off with a different email address. Merging into `main` requires that check to pass.

### If the DCO check fails

For the most recent commit:

```bash
git commit --amend -s --no-edit
git push --force-with-lease
```

For several commits on your branch:

```bash
git rebase --signoff origin/main
git push --force-with-lease
```

## Pull requests

1. Branch from `main`.
2. Use a Conventional Commits title: `feat:`, `fix:`, `docs:`, `chore:`, `refactor:`.
3. Keep each pull request focused on one change.
