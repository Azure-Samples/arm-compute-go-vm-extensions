package main

import (
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"strings"

	"github.com/Azure/azure-sdk-for-go/arm/compute"
	"github.com/Azure/azure-sdk-for-go/arm/disk"
	"github.com/Azure/azure-sdk-for-go/arm/network"
	"github.com/Azure/azure-sdk-for-go/arm/resources/resources"
	"github.com/Azure/azure-sdk-for-go/arm/storage"
	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/azure"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/marstr/guid"
)

var (
	userSubscriptionID string
	userTenantID       string
	environment        = azure.PublicCloud
)

var (
	errLog    *log.Logger
	statusLog *log.Logger
	debugLog  *log.Logger
	wait      bool
)

const (
	clientID = "04b07795-8ddb-461a-bbee-02f9e1bf7b46" // This is the client ID for the Azure CLI. It was chosen for its public well-known status.
	location = "WESTUS2"
)

func main() {
	var group resources.Group
	var sampleVM compute.VirtualMachine
	var sampleNetwork network.VirtualNetwork
	var sampleStorageAccount storage.Account

	var authorizer autorest.Authorizer
	exitStatus := 1
	defer func() {
		os.Exit(exitStatus)
	}()

	debugLog.Println("Using Subscription ID: ", userSubscriptionID)
	debugLog.Println("Using Tenant ID: ", userTenantID)

	// Get authenticated so we can access the subscription used to run this sample.
	if temp, err := authenticate(userTenantID); err == nil {
		authorizer = temp
	} else {
		errLog.Printf("could not authenticate. Error: %v", err)
		return
	}

	// Create a Resource Group to act as a sandbox for this sample.
	if temp, deleter, err := setupResourceGroup(userSubscriptionID, authorizer); err == nil {
		group = temp
		statusLog.Print("Created Resource Group: ", *group.Name)
		defer func() {
			if wait {
				fmt.Print("press ENTER to continue...")
				fmt.Scanln()
			}
			statusLog.Print("Deleting Resource Group: ", *group.Name)
			deleter()
		}()
	} else {
		errLog.Printf("could not create resource group. Error: %v", err)
		return
	}

	// Create Pre-requisites for a VM. Because they are independent, we can do so in parallel.
	storageAccountResults, storageAccountErrs := setupStorageAccount(userSubscriptionID, group, authorizer)
	virtualNetworkResults, virtualNetworkErrs := setupVirtualNetwork(userSubscriptionID, group, authorizer)

	select {
	case sampleNetwork = <-virtualNetworkResults:
	case err := <-virtualNetworkErrs:
		errLog.Print(err)
		return
	}
	statusLog.Print("Created Virtual Network: ", *sampleNetwork.Name)

	select {
	case sampleStorageAccount = <-storageAccountResults:
	case err := <-storageAccountErrs:
		errLog.Print(err)
		return
	}
	statusLog.Print("Created Storage Account: ", *sampleStorageAccount.Name)

	// Create an encrypted Data Disk
	sampleDataDisk, err := setupEncryptedDataDisk(userSubscriptionID, group, sampleStorageAccount, authorizer)
	if err != nil {
		errLog.Print(err)
		return
	}
	// var sampleStorageAccountType compute.StorageAccountTypes
	// switch sampleDisk.AccountType {
	// case disk.StandardLRS:
	// 	sampleStorageAccountType = compute.StandardLRS
	// case disk.PremiumLRS:
	// 	sampleStorageAccountType = compute.PremiumLRS
	// default:
	// 	errLog.Print("Unknown Storage Account Type: ", *sampleStorageAccount.Type)
	// 	return
	// }

	dataDisks := []compute.DataDisk{
		{
			ManagedDisk: &compute.ManagedDiskParameters{
				ID:                 sampleDataDisk.ID,
				StorageAccountType: compute.StorageAccountTypes(sampleDataDisk.AccountType),
			},
		},
	}

	osDisk := compute.OSDisk{
		ManagedDisk: &compute.ManagedDiskParameters{},
	}

	// Create an Azure Virtual Machine, on which we'll mount an encrypted data disk.
	if temp, err := setupVirtualMachine(userSubscriptionID, group, sampleStorageAccount, osDisk, dataDisks, (*sampleNetwork.Subnets)[0], authorizer, nil); err == nil {
		sampleVM = temp
		statusLog.Print("Created Virtual Machine: ", *sampleVM.Name)
	} else {
		errLog.Print(err)
		return
	}

	vmClient := compute.NewVirtualMachinesClient(userSubscriptionID)
	vmClient.Authorizer = authorizer

	exitStatus = 0
}

func init() {
	var badArgs bool

	errLog = log.New(os.Stderr, "[ERROR] ", 0)
	statusLog = log.New(os.Stdout, "[STATUS] ", 0)

	unformattedSubscriptionID := flag.String("subscription", os.Getenv("AZURE_SUBSCRIPTION_ID"), "The subscription that will be targeted when running this sample.")
	unformattedTenantID := flag.String("tenant", os.Getenv("AZURE_TENANT_ID"), "The tenant that hosts the subscription to be used by this sample.")
	printDebug := flag.Bool("debug", false, "Include debug information in the output of this program.")
	flag.BoolVar(&wait, "wait", false, "Use to wait for user acknowledgement before deletion of the created assets.")
	flag.Parse()

	ensureGUID := func(name, raw string) string {
		var retval string
		if parsed, err := guid.Parse(raw); err == nil {
			retval = parsed.String()
		} else {
			errLog.Printf("'%s' doesn't look like an Azure %s. This sample expects a uuid.", raw, name)
			badArgs = true
		}
		return retval
	}

	userSubscriptionID = ensureGUID("Subscription ID", *unformattedSubscriptionID)
	userTenantID = ensureGUID("Tenant ID", *unformattedTenantID)

	var debugWriter io.Writer
	if *printDebug {
		debugWriter = os.Stdout
	} else {
		debugWriter = ioutil.Discard
	}
	debugLog = log.New(debugWriter, "[DEBUG] ", 0)

	if badArgs {
		os.Exit(1)
	}
}

func setupResourceGroup(subscriptionID string, authorizer autorest.Authorizer) (created resources.Group, deleter func(), err error) {
	resourceClient := resources.NewGroupsClient(subscriptionID)
	resourceClient.Authorizer = authorizer

	name := fmt.Sprintf("sample-rg%s", guid.NewGUID().Stringf(guid.FormatN))

	created, err = resourceClient.CreateOrUpdate(name, resources.Group{
		Location: to.StringPtr(location),
	})

	if err == nil {
		deleter = func() {
			_, err = resourceClient.Delete(*created.Name, nil)
			if err == nil {
				return
			}
		}
	} else {
		deleter = func() {}
	}

	return
}

func setupEncryptedDataDisk(subscriptionID string, group resources.Group, account storage.Account, authorizer autorest.Authorizer) (created disk.Model, err error) {
	client := disk.NewDisksClient(subscriptionID)
	client.Authorizer = authorizer

	diskName := "sampleDataDisk"

	_, err = client.CreateOrUpdate(*group.Name, diskName, disk.Model{
		Location: group.Location,
		Properties: &disk.Properties{
			CreationData: &disk.CreationData{
				CreateOption: disk.Empty,
			},
			DiskSizeGB: to.Int32Ptr(64),
		},
	}, nil)
	if err != nil {
		return
	}

	created, err = client.Get(*group.Name, diskName)
	return
}

func setupVirtualMachine(subscriptionID string, resourceGroup resources.Group, storageAccount storage.Account, osDisk compute.OSDisk, dataDisks []compute.DataDisk, subnet network.Subnet, authorizer autorest.Authorizer, cancel <-chan struct{}) (created compute.VirtualMachine, err error) {
	var networkCard network.Interface

	client := compute.NewVirtualMachinesClient(subscriptionID)
	client.Authorizer = authorizer

	vmName := fmt.Sprintf("sample-vm%s", guid.NewGUID().Stringf(guid.FormatN))
	debugLog.Print("VM Name: ", vmName)

	networkCard, err = setupNetworkInterface(subscriptionID, resourceGroup, subnet, network.SubResource{ID: to.StringPtr(vmName)}, authorizer)
	if err != nil {
		return
	}

	debugLog.Print("NIC ID: ", *networkCard.ID)

	if _, err = client.CreateOrUpdate(*resourceGroup.Name, vmName, compute.VirtualMachine{
		Location: resourceGroup.Location,
		VirtualMachineProperties: &compute.VirtualMachineProperties{
			HardwareProfile: &compute.HardwareProfile{
				VMSize: compute.StandardDS1V2,
			},
			NetworkProfile: &compute.NetworkProfile{
				NetworkInterfaces: &[]compute.NetworkInterfaceReference{
					{
						ID: networkCard.ID,
						NetworkInterfaceReferenceProperties: &compute.NetworkInterfaceReferenceProperties{
							Primary: to.BoolPtr(true),
						},
					},
				},
			},
			OsProfile: &compute.OSProfile{
				ComputerName:  to.StringPtr(vmName),
				AdminUsername: to.StringPtr("sampleuser"),
				AdminPassword: to.StringPtr("azureRocksWithGo!"),
				LinuxConfiguration: &compute.LinuxConfiguration{
					DisablePasswordAuthentication: to.BoolPtr(false),
				},
			},
			StorageProfile: &compute.StorageProfile{
				ImageReference: &compute.ImageReference{
					Publisher: to.StringPtr("Canonical"),
					Offer:     to.StringPtr("UbuntuServer"),
					Sku:       to.StringPtr("14.04.5-LTS"),
					Version:   to.StringPtr("latest"),
				},
				OsDisk:    &osDisk,
				DataDisks: &dataDisks,
			},
		},
	}, cancel); err == nil {
		created, err = client.Get(*resourceGroup.Name, vmName, compute.InstanceView)
	}

	if err != nil {
		return
	}
	return
}

func setupVirtualNetwork(subscriptionID string, resourceGroup resources.Group, authorizer autorest.Authorizer) (<-chan network.VirtualNetwork, <-chan error) {
	results, errs := make(chan network.VirtualNetwork), make(chan error)

	go func() {
		defer close(errs)
		defer close(results)

		var err error

		networkClient := network.NewVirtualNetworksClient(subscriptionID)
		networkClient.Authorizer = authorizer

		const networkName = "sampleNetwork"

		_, err = networkClient.CreateOrUpdate(*resourceGroup.Name, networkName, network.VirtualNetwork{
			Location: resourceGroup.Location,
			VirtualNetworkPropertiesFormat: &network.VirtualNetworkPropertiesFormat{
				AddressSpace: &network.AddressSpace{
					AddressPrefixes: &[]string{
						"192.168.0.0/16",
					},
				},
			},
		}, nil)
		if err != nil {
			errs <- err
			return
		}

		subnetClient := network.NewSubnetsClient(subscriptionID)
		subnetClient.Authorizer = authorizer

		const subnetName = "sampleSubnet"

		_, err = subnetClient.CreateOrUpdate(*resourceGroup.Name, networkName, "sampleSubnet", network.Subnet{
			SubnetPropertiesFormat: &network.SubnetPropertiesFormat{
				AddressPrefix: to.StringPtr("192.168.1.0/24"),
			},
		}, nil)
		if err != nil {
			errs <- err
			return
		}

		created, err := networkClient.Get(*resourceGroup.Name, networkName, "")
		if err != nil {
			errs <- err
			return
		}

		results <- created
	}()

	return results, errs
}

func setupNetworkInterface(subscriptionID string, resourceGroup resources.Group, subnet network.Subnet, machine network.SubResource, authorizer autorest.Authorizer) (created network.Interface, err error) {
	client := network.NewInterfacesClient(subscriptionID)
	client.Authorizer = authorizer

	var ip network.PublicIPAddress

	ip, err = setupPublicIP(subscriptionID, resourceGroup, authorizer)
	if err != nil {
		return
	}

	statusLog.Print("Created Public IP Address: ", *ip.Name, " ", *ip.IPAddress)

	name := "sample-networkInterface"

	_, err = client.CreateOrUpdate(*resourceGroup.Name, name, network.Interface{
		Location: resourceGroup.Location,
		InterfacePropertiesFormat: &network.InterfacePropertiesFormat{
			IPConfigurations: &[]network.InterfaceIPConfiguration{
				{
					Name: to.StringPtr(fmt.Sprintf("ipConfig-%s", *machine.ID)),
					InterfaceIPConfigurationPropertiesFormat: &network.InterfaceIPConfigurationPropertiesFormat{
						PrivateIPAllocationMethod: network.Dynamic,
						Primary:                   to.BoolPtr(true),
						PublicIPAddress:           &ip,
						Subnet:                    &subnet,
					},
				},
			},
		},
	}, nil)
	if err != nil {
		return
	}

	created, err = client.Get(*resourceGroup.Name, name, "")
	return
}

func setupNetworkSecurityGroup(subscriptionID, resourceGroupName string, authorizer autorest.Authorizer) (created network.SecurityGroup, err error) {
	client := network.NewSecurityGroupsClient(subscriptionID)
	client.Authorizer = authorizer

	name := "sample-nsg"

	_, err = client.CreateOrUpdate(resourceGroupName, name, network.SecurityGroup{
		Location:                      to.StringPtr(location),
		SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{},
	}, nil)
	if err != nil {
		return
	}

	created, err = client.Get(resourceGroupName, name, "")
	return
}

func setupPublicIP(subscriptionID string, group resources.Group, authorizer autorest.Authorizer) (created network.PublicIPAddress, err error) {
	client := network.NewPublicIPAddressesClient(subscriptionID)
	client.Authorizer = authorizer

	name := "sample-publicip"

	_, err = client.CreateOrUpdate(*group.Name, name, network.PublicIPAddress{
		Location: group.Location,
		PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: network.Static,
		},
	}, nil)
	if err != nil {
		return
	}

	created, err = client.Get(*group.Name, name, "")
	return
}

func setupStorageAccount(subscriptionID string, group resources.Group, authorizer autorest.Authorizer) (<-chan storage.Account, <-chan error) {
	results, errs := make(chan storage.Account), make(chan error)

	go func() {
		defer close(errs)
		defer close(results)

		client := storage.NewAccountsClient(subscriptionID)
		client.Authorizer = authorizer

		storageAccountName := "sample"
		storageAccountName = storageAccountName + string([]byte(guid.NewGUID().Stringf(guid.FormatN))[:24-len(storageAccountName)])
		storageAccountName = strings.ToLower(storageAccountName)
		debugLog.Printf("storageAccountName (length: %d): %s", len(storageAccountName), storageAccountName)

		_, err := client.Create(*group.Name, storageAccountName, storage.AccountCreateParameters{
			Location: group.Location,
			Sku: &storage.Sku{
				Name: storage.StandardLRS,
			},
		}, nil)
		if err != nil {
			errs <- err
			return
		}

		result, err := client.GetProperties(*group.Name, storageAccountName)
		if err != nil {
			errs <- err
			return
		}
		results <- result
	}()

	return results, errs
}

// authenticate gets an authorization token to allow clients to access Azure assets.
func authenticate(tenantID string) (autorest.Authorizer, error) {
	authClient := autorest.NewClientWithUserAgent("github.com/Azure-Samples/arm-compute-go-vm-extensions")
	var deviceCode *azure.DeviceCode
	var token *azure.Token
	var config *azure.OAuthConfig

	if temp, err := environment.OAuthConfigForTenant(tenantID); err == nil {
		config = temp
	} else {
		return nil, err
	}

	debugLog.Print("DeviceCodeEndpoint: ", config.DeviceCodeEndpoint.String())
	if temp, err := azure.InitiateDeviceAuth(&authClient, *config, clientID, environment.ServiceManagementEndpoint); err == nil {
		deviceCode = temp
	} else {
		return nil, err
	}

	if _, err := fmt.Println(*deviceCode.Message); err != nil {
		return nil, err
	}

	if temp, err := azure.WaitForUserCompletion(&authClient, deviceCode); err == nil {
		token = temp
	} else {
		return nil, err
	}

	return token, nil
}
