-- The stock hwmon collector skips *_alarm files. On a Raspberry Pi,
-- rpi_volt's in0_lcrit_alarm is the firmware's current under-voltage flag.
local fs = require "nixio.fs"
local nixio = require "nixio"
local json = require "luci.jsonc"
local state_path = "/tmp/slopify-hwmon-alarms"

-- uhttpd can fork collectors. Serialize independent scrapers and retain the last
-- observation across exporter restarts, without writing to flash. Reboot resets
-- this sampled-transition counter; dips between scrapes remain unobservable.
local function events(observations)
  if #observations == 0 then return end
  local ok, result = pcall(function()
    local state = {}
    local file = io.open(state_path .. ".json", "rb")
    if file then
      local data = file:read(65537)
      file:close()
      if data and #data <= 65536 then state = json.parse(data) or {} end
    end
    if type(state) ~= "table" then state = {} end
    local changed = false
    for _, observation in ipairs(observations) do
      local key = observation.chip .. "/" .. observation.sensor
      local previous = state[key]
      if type(previous) ~= "table" or type(previous.count) ~= "number" or
         previous.count < 0 or previous.count > 9007199254740990 then
        previous = {count=0, active=false}
      end
      local active = observation.value ~= 0
      local count = previous.count
      if active and not previous.active then count = count + 1 end
      changed = changed or state[key] == nil or active ~= previous.active or count ~= previous.count
      state[key] = {count=count, active=active}
      observation.count = count
    end
    if changed then
      local data = assert(json.stringify(state))
      assert(#data <= 65536, "hwmon event state too large")
      local file = assert(nixio.open(state_path .. ".tmp", "w", "600"))
      local written = file:writeall(data)
      file:close()
      assert(written, "cannot write hwmon event state")
      assert(fs.rename(state_path .. ".tmp", state_path .. ".json"))
    end
    return observations
  end)
  if not ok then error(result) end
  local emit = metric("node_hwmon_alarm_events_total", "counter")
  for _, observation in ipairs(result) do
    emit({chip=observation.chip, sensor=observation.sensor}, observation.count)
  end
end

local function scrape_locked()
  local metric_alarm = metric("node_hwmon_alarm", "gauge")
  local observations = {}
  for hwmon_path in fs.glob("/sys/class/hwmon/hwmon*") do
    local chip = string.gsub(get_contents(hwmon_path .. "/name"), "\n$", "")
    if chip == "" then
      chip = fs.basename(hwmon_path)
    end
    local real_dev_path, status = fs.realpath(hwmon_path .. "/device")
    if not status then
      chip = fs.basename(fs.dirname(real_dev_path)) .. "_" .. fs.basename(real_dev_path)
    end
    for alarm_path in fs.glob(hwmon_path .. "/*_alarm") do
      local value = tonumber(get_contents(alarm_path))
      if value then
        local sensor = string.gsub(fs.basename(alarm_path), "_alarm$", "")
        metric_alarm({chip=chip, sensor=sensor}, value)
        if #observations < 64 then
          observations[#observations+1] = {chip=chip, sensor=sensor, value=value}
        end
      end
    end
  end
  events(observations)
end

local function scrape()
  local lock = assert(nixio.open(state_path .. ".lock", "a+", "600"))
  if not lock:lock("tlock") then
    lock:close()
    error("hwmon event state busy")
  end
  local ok, err = pcall(scrape_locked)
  lock:close()
  if not ok then error(err) end
end

return { scrape = scrape }
