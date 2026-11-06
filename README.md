# SMD parts manager

Shared inventory for chip resistors, MLCC, tantalum capacitors, coils, and diodes. The Korean guide is [README_KO.md](README_KO.md).

The live database is `data/smd_parts.db` (103 parts). The server binds `0.0.0.0:$PORT` (default 8080) and does not import `seed/parts.json`.

```sh
go run .
```

Open http://127.0.0.1:8080 . `render.yaml` is a free Render web service with no disk. A restart puts the database back to the 103 rows baked into the image. There is no public URL in this repository.
