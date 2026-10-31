#!/bin/bash
# Build and install MyThermostat on the Raspberry Pi. Run from the git
# checkout as a user with sudo.
set -e

INSTALL_USER="myhomeservices"
INSTALL_DIR="/home/$INSTALL_USER/MyThermostat"
SERVICE_NAME="thermostat"

echo "Building binary..."
go build -o MyThermostat .

# When the checkout is the install directory, only the binary is new
if [ "$(realpath .)" != "$(realpath -m "$INSTALL_DIR")" ]; then
    sudo mkdir -p "$INSTALL_DIR/static"
    sudo cp MyThermostat "$INSTALL_DIR/"
    sudo cp static/* "$INSTALL_DIR/static/"
fi
if [ ! -f "$INSTALL_DIR/config.yaml" ]; then
    sudo cp config.example.yaml "$INSTALL_DIR/config.yaml"
    echo "⚠️  Edit $INSTALL_DIR/config.yaml with your American Standard Home login before starting."
fi
sudo chown -R "$INSTALL_USER:$INSTALL_USER" "$INSTALL_DIR"
sudo chmod 600 "$INSTALL_DIR/config.yaml"

sudo cp "$SERVICE_NAME.service" /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable "$SERVICE_NAME"
sudo systemctl restart "$SERVICE_NAME"

echo "MyThermostat installed. Logs: sudo journalctl -u $SERVICE_NAME -f"
