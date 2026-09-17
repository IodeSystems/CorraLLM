# staging log archive

Logs pulled off **our own** staging box (`stg-api-01`, `stg-worker-02`) for the
quarterly credential rotation. Nothing in this directory came from a customer,
a third party, or production.

Staging leaks secrets into logs because staging runs with `LOG_LEVEL=debug` and
a config dumper that nobody has fixed. That is the problem we are trying to
find, not a problem we are trying to hide.

See `ROTATION.md` for what the on-call does with the report.
