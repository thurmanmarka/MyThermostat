# MyThermostat

Control an American Standard Wi-Fi thermostat (Gold XL824, AccuLink Platinum 850/1050 and the like) from MyHomeServicesHub:

- **Status and control:** temperature, humidity, outdoor temperature, what the system is doing, mode, heat and cool setpoints, hold or follow the schedule, for each zone.
- **Settings:** every other setting the thermostat offers with a list of choices, such as fan mode, fan speed, emergency heat, air cleaner and presets. The thermostat decides which ones there are.
- **History:** indoor, outdoor and setpoint chart, and heating and cooling run time per day, kept in MariaDB.
- **Schedules:** changes that run from the Pi at set times, such as pre-cooling before peak electricity rates. These come on top of the thermostat's own schedule.
- **Change log:** who changed what, from the page or a schedule.

## How it talks to the thermostat

The XL824 has no local API. MyThermostat signs in to the **American Standard Home cloud**, the same way the phone app does, and reads it every two minutes. It works anywhere the Pi has internet, whatever network the thermostat is on.

There is no official API. The calls follow the open-source [nexia](https://github.com/bdraco/nexia) library that Home Assistant uses. If American Standard changes their API, this needs updating to match.

The login is kept in `config.yaml`. If the app lets you invite another user to your home, give the Pi its own account. Then you can remove it without changing your password.

The cloud locks an account after a few bad passwords. After a failed sign-in, MyThermostat waits 15 minutes before trying again, doubling the wait each time up to 6 hours.

## Set up on the Pi

1. **Create the database,** as the MariaDB root user:
   ```sql
   CREATE DATABASE hvac_db;
   CREATE USER 'hvac_app'@'localhost' IDENTIFIED BY '<a generated password>';
   GRANT ALL PRIVILEGES ON hvac_db.* TO 'hvac_app'@'localhost';
   ```
   The tables are created on first start.

2. **Build it,** as `myhomeservices`:
   ```bash
   git clone https://github.com/thurmanmarka/MyThermostat.git ~/MyThermostat
   cd ~/MyThermostat
   go build -o MyThermostat .
   cp config.example.yaml config.yaml && chmod 600 config.yaml
   nano config.yaml          # your American Standard Home login and the DB password
   ```

3. **Check the sign-in** and see what your thermostat offers:
   ```bash
   ./MyThermostat -check
   ```
   `-dump` prints the raw device data from the cloud, if something looks wrong.

4. **Install the service,** as a user with sudo:
   ```bash
   sudo cp /home/myhomeservices/MyThermostat/thermostat.service /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl enable --now thermostat
   sudo journalctl -u thermostat -f
   ```

5. **Add it to the hub:** in the hub's `config.yaml`, under `services:`, add the tile below. Indent it with 2 spaces, like the other tiles. The hub already sends `/hvac` to port 8086.
   ```yaml
     - name: Thermostat
       description: Temperature, modes and schedules for the HVAC
       icon: 🌡️
       path: /hvac
       enabled: true
       allowed_roles:
         - admin
         - user
   ```

**Updating later:** `git pull && go build -o MyThermostat .` as `myhomeservices`, then `sudo systemctl restart thermostat`.

## Notes

- **Setpoints:** changing one keeps the other at least the thermostat's deadband away, as the thermostat itself does. Fahrenheit setpoints are whole degrees; Celsius ones are rounded to the nearest half degree.
- **Holds:** a setpoint change on a schedule lasts until the thermostat's next schedule period. Use **Hold** to keep it.
- **Run time** comes from the readings: each reading's heating or cooling state is counted until the next one. It's accurate to about the poll interval.
- **Running it on a PC to test:** set `allow_direct: true`. Then open `http://127.0.0.1:8086/`.
