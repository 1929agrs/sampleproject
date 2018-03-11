Vagrant.configure(2) do |config|
  unless Vagrant.has_plugin?('vagrant-hostmanager')
    puts "Run `vagrant plugin install vagrant-hostmanager` to continue setup"
    exit 1
  end

  config.vm.box = "ubuntu/xenial64"
  config.vm.network 'forwarded_port', guest: 8181, host: 8181

  config.vm.provider "virtualbox" do |v|
    v.memory = 256
    v.cpus = 1
  end

  config.hostmanager.enabled = true
  config.hostmanager.manage_host = false
  config.hostmanager.manage_guest = true
  config.hostmanager.include_offline = true
  config.hostmanager.ip_resolver = proc do |vm, resolving_vm|
    (Socket.ip_address_list.detect{|intf| intf.ipv4_private?}).inspect_sockaddr
  end

  config.vm.define "wstunnel" do |dev|
    dev.hostmanager.aliases = %w(etcd.local elastic.local)
    dev.vm.provision "ansible" do |ansible|
      ansible.playbook = "provisioning/dev.yml"
      ansible.verbose = "vvv"
    end
  end

  if Vagrant.has_plugin?('vagrant-cachier')
    config.cache.scope = :box
  else
    puts "Run `vagrant plugin install vagrant-cachier` to reduce caffeine intake when provisioning"
  end
end
