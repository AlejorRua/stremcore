# Actualizacion diaria sin base de datos

El workflow `.github/workflows/update-catalog.yml` usa los JSON del propio
repositorio como fuente de verdad. No necesita PostgreSQL, Supabase ni secrets.

Cada dia hace lo siguiente:

1. Descarga el catalogo actual.
2. Revisa la portada de FlixLatam.
3. Compara peliculas y series por slug, titulo y año.
4. Comprueba episodios por serie, temporada y numero.
5. Agrega solo los detalles nuevos y fusiona servidores por URL.
6. Regenera `pages/`, `search/` y `meta.json` desde los JSON completos.
7. Crea un commit solamente cuando existen cambios.

Los proveedores que ya estan publicados no se borran. Para una pelicula o un
episodio existente, los servidores nuevos se agregan despues de los anteriores
y se eliminan URLs duplicadas.

## Probar localmente

Compila el actualizador:

```bash
cd scraper
go build -o flixlatam-monitor .
```

Detecta novedades sin escribir nada:

```bash
./flixlatam-monitor update-json --root .. --dry-run
```

Ejecuta una actualizacion real:

```bash
./flixlatam-monitor update-json --root ..
cd ..
python3 doc/rebuild_catalog_indexes.py
python3 doc/generate_search_index.py
```

## Activar GitHub Actions

Sube el codigo a la rama predeterminada:

```bash
git add .github .gitignore GITHUB_ACTIONS.md doc scraper state
git commit -m "add database-free catalog updater"
git push origin main
```

Despues abre `Actions > Actualizar catalogo FlixLatam > Run workflow` para la
primera prueba. El horario automatico es `03:17 UTC`.

El flag `--max-items N` limita cuantos elementos de cada tipo procesa una
ejecucion y es util para pruebas en una copia temporal.
