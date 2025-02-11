# hiro-cross-cluster-workloads
`hiro-cross-cluster-workloads` is designed to enable cross-cluster workload (Pod) management using a work-stealing algorithm. It has two main components:
1. Stealer
2. Donor

There are multiple clusters, each with its own responsibility as either a stealer or a donor.

The stealer always tries to steal workloads from donors based on certain intelligence. If the stealer successfully steals a pod from a donor, a placeholder pod will be placed in the donor in a Pending state. When the stolen pod completes its execution (terminates softly), the execution results will be published to the respective donor so that the donor can delete its placeholder pod and take the necessary steps based on the results.

To install the stealer, follow these steps:
1. **Clone the Repository**:
    Clone the `hiro-cross-cluster-workloads` repository to your local machine using the following command:
    ```sh
    git clone https://github.com/HIRO-MicroDataCenters-BV/hiro-cross-cluster-workloads.git
    cd hiro-cross-cluster-workloads
    chmod +x scripts/stealer/*
    ```

2. **Start the Stealer**:
   Run the `start_stealer.sh` script to execute both the `initialize.sh` and `install.sh` scripts sequentially. This script ensures that the initialization and installation steps are completed successfully.
   ```sh
   ./start_stealer.sh
   ```

    **Redeploy the Stealer**:
    If you need to redeploy the stealer server, you can use the `redeploy.sh` script. This script will rebuild the Docker image and redeploy the worker server without reinitializing the Kind cluster.
    ```sh
    cd scripts/stealer
    ./redeploy.sh
    ```

To install the donor, follow these steps:
1. **Clone the Repository**:
    Clone the `hiro-cross-cluster-workloads` repository to your local machine using the following command:
    ```sh
    git clone https://github.com/HIRO-MicroDataCenters-BV/hiro-cross-cluster-workloads.git
    cd hiro-cross-cluster-workloads
    chmod +x scripts/donor/*
    ```
2. **Run the `start_donor.sh` script**:
    The `start_donor.sh` script initializes the Kind cluster, builds and installs the application, and sets up the necessary configurations to mark the pods as donors.
    ```sh
    ./start_donor.sh
    ```

    **Redeploy the Donor**:
    If you need to redeploy the donor server, you can use the `redeploy.sh` script. This script will rebuild the Docker image and redeploy the worker server without reinitializing the Kind cluster.
    ```sh
    cd scripts/donor
    ./redeploy.sh
    ```

3. **Test by deploying a pod**:
    Once the above components are installed, test them by running `scripts/test_pod_steal.sh`
    ```sh
    cd scripts
    ./test_pod_steal.sh
    ```
