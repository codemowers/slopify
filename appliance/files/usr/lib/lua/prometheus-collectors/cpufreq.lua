-- CPUFreq policies can cover several CPUs; export each policy once.
-- scaling_cur_freq is the driver's reported frequency, not a measured clock.
local fs = require "nixio.fs"

local function scrape()
  local frequencies = {
    scaling_cur_freq = metric("node_cpu_scaling_frequency_hertz", "gauge"),
    scaling_min_freq = metric("node_cpu_scaling_frequency_min_hertz", "gauge"),
    scaling_max_freq = metric("node_cpu_scaling_frequency_max_hertz", "gauge"),
  }
  local governor_info = metric("node_cpu_scaling_governor_info", "gauge")
  for path in fs.glob("/sys/devices/system/cpu/cpufreq/policy*") do
    local policy = fs.basename(path)
    if policy:match("^policy%d+$") then
      for file, emit in pairs(frequencies) do
        local value = tonumber(get_contents(path .. "/" .. file))
        if value and value > 0 then
          emit({policy=policy}, value * 1000) -- sysfs uses kHz.
        end
      end
      local governor = (get_contents(path .. "/scaling_governor") or ""):match("^%s*([%w_-]+)%s*$")
      if governor and #governor <= 64 then
        governor_info({policy=policy, governor=governor}, 1)
      end
    end
  end
end

return { scrape = scrape }
