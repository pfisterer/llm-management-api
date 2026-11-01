# dhbw-cloud

Client of the DHBW Cloud's AI service (KI-Dienst) and the command `dhbw-cloud`: `dhbw-cloud login` once, then `dhbw-cloud job …` for batch jobs on its GPUs; more areas of the API will follow as subcommands.

```
pip install dhbw-cloud
dhbw-cloud login                          # personal API token (aiaas_…) from the self-service UI
dhbw-cloud job submit --project t-… -- python train.py --epochs 10
dhbw-cloud job list
dhbw-cloud job log -f j-4f3a9c0d2e
dhbw-cloud job cancel j-4f3a9c0d2e
```

A job runs in the project's environment with the home and `~/project-shared` of the person, on one GPU of a class the project has; its output also lands in `~/jobs/<name>/output-<attempt>.log`. Notebooks run with papermill: `dhbw-cloud job submit --project t-… --notebook kurs/training.ipynb`. Whether a project has batch jobs, how many per person, how long they may run and whether a workspace may evict them is set in the project's budget.

From Python, the generated client of the whole API is `dhbw_cloud.api`:

```python
import dhbw_cloud
from dhbw_cloud.api.api.gpu import list_gpu_jobs

for job in list_gpu_jobs.sync(client=dhbw_cloud.client()):
    print(job.id, job.state)
```

The address is `DHBW_CLOUD_URL` (default production), the token `DHBW_CLOUD_TOKEN` or the file `~/.config/dhbw-cloud/token`.
