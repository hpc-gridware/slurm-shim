/*___INFO__MARK_BEGIN__*/
/*************************************************************************
*  Copyright 2026 HPC-Gridware GmbH
*
*  Licensed under the Apache License, Version 2.0 (the "License");
*  you may not use this file except in compliance with the License.
*  You may obtain a copy of the License at
*
*      http://www.apache.org/licenses/LICENSE-2.0
*
*  Unless required by applicable law or agreed to in writing, software
*  distributed under the License is distributed on an "AS IS" BASIS,
*  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
*  See the License for the specific language governing permissions and
*  limitations under the License.
*
************************************************************************/
/*___INFO__MARK_END__*/

/*
 * The MPI program of test/e2e/35_mpi_pmix.sh: every rank reports its rank,
 * the world size, its host, the CPUs it may run on and the result of an
 * all-reduce over all ranks (a wired-up job prints sum=size), plus the
 * SLURM_PROCID it was started with. With an argument N, rank N exits 3.
 */
#include <mpi.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

static void cpus_allowed(char *buf, size_t n) {
    FILE *f = fopen("/proc/self/status", "r");
    char line[512];
    buf[0] = 0;
    if (!f) return;
    while (fgets(line, sizeof line, f)) {
        if (strncmp(line, "Cpus_allowed_list:", 18) == 0) {
            char *v = line + 18;
            while (*v == ' ' || *v == '\t') v++;
            v[strcspn(v, "\n")] = 0;
            snprintf(buf, n, "%s", v);
        }
    }
    fclose(f);
}

int main(int argc, char **argv) {
    int rank, size, sum = 0, one = 1;
    char host[256], cpus[256];
    const char *procid = getenv("SLURM_PROCID");
    MPI_Init(&argc, &argv);
    MPI_Comm_rank(MPI_COMM_WORLD, &rank);
    MPI_Comm_size(MPI_COMM_WORLD, &size);
    gethostname(host, sizeof host);
    cpus_allowed(cpus, sizeof cpus);
    MPI_Allreduce(&one, &sum, 1, MPI_INT, MPI_SUM, MPI_COMM_WORLD);
    printf("HELLO rank=%d size=%d host=%s cpus=%s sum=%d slurm_procid=%s\n",
           rank, size, host, cpus, sum, procid ? procid : "-");
    fflush(stdout);
    if (argc > 1 && atoi(argv[1]) == rank) {
        MPI_Finalize();
        return 3;
    }
    MPI_Finalize();
    return 0;
}
