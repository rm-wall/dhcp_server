# Go DHCP Server

A simple, file-based configurable DHCP server written in Go. It's designed to be lightweight and easy to deploy for small networks, offering both dynamic IP address allocation and static reservations.

## Features

-   Dynamic IP address assignment from a predefined range.
-   Static IP reservations based on MAC addresses.
-   Configurable lease duration.
-   Easy configuration via a single YAML file.
-   Binds to a specific network interface.

## Getting Started

### Prerequisites

-   Go 1.24 or later.
-   Root/administrator privileges to run the server (as it needs to bind to port 67).

### Installation

#### Recommended: Build from source

For safety and compatibility, it is recommended to build the binary yourself:

1.  **Clone the repository:**
    ```sh
    git clone <your-repository-url>
    cd dhcp_server
    ```

2.  **Build the binary:**
    ```sh
    go build -o dhcp_server .
    ```
    This will create a `dhcp_server` executable in the current directory.

#### Optional: Download pre-built Release binaries

You can also download pre-built binaries from the [Releases](https://github.com/rm-wall/dhcp_server/releases) page.  
**Note for macOS users:** Gatekeeper may warn:

````

Apple could not verify “dhcp-server” is free of malware that may harm your Mac or compromise your privacy.

````

This warning appears because the binary is unsigned and not notarized by Apple.  
To run the downloaded binary on macOS:

1. Right-click the binary → "Open" → Click "Open Anyway"
2. Or in the terminal:
    ```sh
    xattr -d com.apple.quarantine dhcp_server
    ```

> ⚠️ Strongly recommended: compiling from source avoids this warning and ensures you are running the latest code.

---

## Usage

The server must be run with `sudo` because it needs to bind to the privileged DHCP port (67).

```sh
sudo ./dhcp_server [options]
````

### Command-line Flags

* `-config <path>`: Specifies the path to the configuration file.

    * Default: `dhcp_config.yaml`
* `-iface <name>`: Specifies the network interface for the server to listen on.

    * Default: `en5`

### Example

* Run with default settings:

  ```sh
  sudo ./dhcp_server
  ```

* Run on a different network interface (`en0`) with a custom config file path:

  ```sh
  sudo ./dhcp_server -iface en0 -config /etc/dhcp/config.yaml
  ```

## Configuration

The server is configured using a YAML file. By default, it looks for `dhcp_config.yaml` in the same directory.

### Example `dhcp_config.yaml`

```yaml
# Network interface to bind to (optional, can be overridden by -iface flag)
interface: "en0"

# Network CIDR (e.g., 192.168.2.0/24)
network: "192.168.2.0/24"

# Gateway IP address (optional)
gateway: "192.168.2.1"

# IP address range for dynamic allocation (format: start-end)
range: "192.168.2.100-192.168.2.200"

# DHCP lease duration in seconds
lease_duration: 3600

# DNS servers to provide to clients (optional)
dns_servers:
  - "8.8.8.8"
  - "8.8.4.4"

# Static IP address reservations based on MAC address (optional)
reserved_addresses:
  "11:22:33:44:55:66": "192.168.2.211"
  "aa:bb:cc:dd:ee:ff": "192.168.2.50"
```

### Parameters

* `interface`: (Optional) Network interface to bind to. Can be overridden by the `-iface` command-line flag.
* `network`: (Required) The subnet in CIDR notation (e.g., `192.168.2.0/24`).
* `gateway`: (Optional) The gateway IP address to advertise to DHCP clients.
* `range`: (Required) The IP address range for dynamic allocation in the format `start-end` (e.g., `192.168.2.100-192.168.2.200`).
* `lease_duration`: (Required) The default time in seconds that an IP address is leased to a client.
* `dns_servers`: (Optional) A list of DNS server IP addresses to provide to clients.
* `reserved_addresses`: (Optional) A map where the key is the client's MAC address (as a string) and the value is the static IP address to assign. Reserved IPs are excluded from the dynamic pool.

## Dependencies

* [github.com/insomniacslk/dhcp](https://github.com/insomniacslk/dhcp) for the underlying DHCP protocol handling.
* [gopkg.in/yaml.v3](https://gopkg.in/yaml.v3) for parsing the configuration file.

## How It Works

The server listens for DHCP DISCOVER and REQUEST packets on the specified interface.

1. When a **DISCOVER** packet is received, the server determines the appropriate IP address for the client:

    * If the client's MAC address is in the `reserved_addresses` map, it offers the corresponding IP.
    * If the client has a previous lease, it attempts to offer the same IP again.
    * Otherwise, it offers an available IP from the dynamic pool.
2. When a **REQUEST** packet is received, the server finalizes the lease, confirms the IP assignment with an ACK packet, and records the lease details.
3. Expired leases are automatically cleaned up and their IP addresses are returned to the available pool.

## Contributing

Contributions are welcome! Please feel free to submit a pull request or open an issue for any bugs, feature requests, or improvements.

## License

This project is free to use under the MIT License.
