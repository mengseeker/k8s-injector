#!/bin/env ruby
require "base64"
puts Base64.encode64 File.open("resources/server.crt").read