"""Materialize recorded local candidate commits without modifying their checkouts."""
import json, subprocess, tarfile, io
from pathlib import Path
for name, commit in json.loads(Path('candidate-sources.json').read_text()).items():
    destination=Path('.tools/candidates')/name
    destination.mkdir(parents=True,exist_ok=True)
    data=subprocess.check_output(['git','-C','../'+name,'archive',commit])
    with tarfile.open(fileobj=io.BytesIO(data)) as archive:
        archive.extractall(destination,filter='data')
